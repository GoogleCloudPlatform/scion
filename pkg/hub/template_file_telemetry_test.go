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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Tests for ptone/scion#2093: a template upload reads the telemetry block of
// scion-agent.yaml into Config.Telemetry when it is unset, a value set
// through the JSON API wins, and telemetry that came from a previous upload
// is cleared when a later scion-agent.yaml drops it.

const (
	yamlTelemetryOff = "harness_config: claude\ntelemetry:\n  enabled: false\n  cloud:\n    endpoint: otel.example.com:4317\n"
	yamlTelemetryOn  = "harness_config: claude\ntelemetry:\n  enabled: true\n"
	yamlNoTelemetry  = "harness_config: claude\n"
)

// templateUploadPaths are the three handlers that accept a scion-agent.yaml.
var templateUploadPaths = map[string]func(t *testing.T, srv *Server, tmplID, content string){
	"json write": func(t *testing.T, srv *Server, tmplID, content string) {
		t.Helper()
		body, err := json.Marshal(map[string]string{"content": content})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPut, "/api/v1/templates/"+tmplID+"/files/scion-agent.yaml", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		serveTemplateUpload(t, srv, req)
	},
	"raw write": func(t *testing.T, srv *Server, tmplID, content string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/api/v1/templates/"+tmplID+"/files/scion-agent.yaml", strings.NewReader(content))
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		serveTemplateUpload(t, srv, req)
	},
	"multipart upload": func(t *testing.T, srv *Server, tmplID, content string) {
		t.Helper()
		serveTemplateUpload(t, srv, templateMultipartRequest(t, tmplID, map[string][]byte{"scion-agent.yaml": []byte(content)}))
	},
}

func serveTemplateUpload(t *testing.T, srv *Server, req *http.Request) {
	t.Helper()
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func storedTelemetry(t *testing.T, s store.Store, id string) *api.TelemetryConfig {
	t.Helper()
	got, err := s.GetTemplate(context.Background(), id)
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	if got.Config == nil {
		return nil
	}
	return got.Config.Telemetry
}

// setJSONTelemetry stores telemetry on the template the way the JSON API
// does (a direct Config update, independent of any file).
func setJSONTelemetry(t *testing.T, s store.Store, tmpl *store.Template, enabled bool) {
	t.Helper()
	got, err := s.GetTemplate(context.Background(), tmpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.Config = &store.TemplateConfig{Model: "opus", Telemetry: &api.TelemetryConfig{Enabled: &enabled}}
	if err := s.UpdateTemplate(context.Background(), got); err != nil {
		t.Fatal(err)
	}
}

func TestTemplateUpload_YAMLFillsUnsetTelemetry(t *testing.T) {
	for name, upload := range templateUploadPaths {
		t.Run(name, func(t *testing.T) {
			srv, s, stor := testTemplateFileServer(t)
			tmpl := createTestTemplate(t, s, stor, map[string]string{"scion-agent.yaml": yamlNoTelemetry})

			upload(t, srv, tmpl.ID, yamlTelemetryOff)

			got := storedTelemetry(t, s, tmpl.ID)
			if got == nil || got.Enabled == nil || *got.Enabled {
				t.Fatalf("expected telemetry enabled=false from the YAML, got %+v", got)
			}
			if got.Cloud == nil || got.Cloud.Endpoint != "otel.example.com:4317" {
				t.Errorf("expected cloud endpoint from the YAML, got %+v", got.Cloud)
			}
		})
	}
}

func TestTemplateUpload_JSONTelemetryWins(t *testing.T) {
	for name, upload := range templateUploadPaths {
		t.Run(name, func(t *testing.T) {
			srv, s, stor := testTemplateFileServer(t)
			tmpl := createTestTemplate(t, s, stor, map[string]string{"scion-agent.yaml": yamlNoTelemetry})
			setJSONTelemetry(t, s, tmpl, true)

			upload(t, srv, tmpl.ID, yamlTelemetryOff)

			got := storedTelemetry(t, s, tmpl.ID)
			if got == nil || got.Enabled == nil || !*got.Enabled || got.Cloud != nil {
				t.Fatalf("expected the JSON-set telemetry (enabled=true) to be kept, got %+v", got)
			}
		})
	}
}

func TestTemplateUpload_LaterUploadClearsYAMLTelemetry(t *testing.T) {
	for name, upload := range templateUploadPaths {
		t.Run(name, func(t *testing.T) {
			srv, s, stor := testTemplateFileServer(t)
			tmpl := createTestTemplate(t, s, stor, map[string]string{"scion-agent.yaml": yamlNoTelemetry})

			upload(t, srv, tmpl.ID, yamlTelemetryOff)
			if storedTelemetry(t, s, tmpl.ID) == nil {
				t.Fatal("fixture: first upload did not set telemetry")
			}
			// A changed block replaces the file-sourced value.
			upload(t, srv, tmpl.ID, yamlTelemetryOn)
			if got := storedTelemetry(t, s, tmpl.ID); got == nil || got.Enabled == nil || !*got.Enabled || got.Cloud != nil {
				t.Fatalf("expected the second file's telemetry, got %+v", got)
			}
			// Dropping the block clears it.
			upload(t, srv, tmpl.ID, yamlNoTelemetry)
			if got := storedTelemetry(t, s, tmpl.ID); got != nil {
				t.Errorf("expected file-sourced telemetry to be cleared, got %+v", got)
			}
		})
	}
}

func TestTemplateUpload_LaterUploadKeepsJSONTelemetry(t *testing.T) {
	for name, upload := range templateUploadPaths {
		t.Run(name, func(t *testing.T) {
			srv, s, stor := testTemplateFileServer(t)
			// The stored file has a telemetry block that differs from the
			// JSON-set value, so the JSON value is not file-sourced.
			tmpl := createTestTemplate(t, s, stor, map[string]string{"scion-agent.yaml": yamlTelemetryOff})
			setJSONTelemetry(t, s, tmpl, true)

			upload(t, srv, tmpl.ID, yamlNoTelemetry)

			got := storedTelemetry(t, s, tmpl.ID)
			if got == nil || got.Enabled == nil || !*got.Enabled {
				t.Fatalf("expected the JSON-set telemetry to survive, got %+v", got)
			}
			full, err := s.GetTemplate(context.Background(), tmpl.ID)
			if err != nil {
				t.Fatal(err)
			}
			if full.Config.Model != "opus" {
				t.Errorf("other config fields changed: %+v", full.Config)
			}
		})
	}
}

// Known limit, pinned: a JSON-set value equal to the replaced file's
// telemetry block cannot be told apart from a file-sourced one, so it
// follows the file and is cleared when the new file drops the block. Only
// the file-handler upload paths are covered; finalize is ptone/scion#4125.
func TestTemplateUpload_JSONTelemetryEqualToFileFollowsFile(t *testing.T) {
	for name, upload := range templateUploadPaths {
		t.Run(name, func(t *testing.T) {
			srv, s, stor := testTemplateFileServer(t)
			// The stored file's block (enabled: true) equals the JSON value.
			tmpl := createTestTemplate(t, s, stor, map[string]string{"scion-agent.yaml": yamlTelemetryOn})
			setJSONTelemetry(t, s, tmpl, true)

			upload(t, srv, tmpl.ID, yamlNoTelemetry)

			if got := storedTelemetry(t, s, tmpl.ID); got != nil {
				t.Errorf("expected the value equal to the replaced file to be cleared, got %+v", got)
			}
		})
	}
}

// Content that does not parse leaves the stored telemetry alone.
func TestApplyAgentConfigUpload_UnparseableKeepsTelemetry(t *testing.T) {
	enabled := false
	prev := &api.TelemetryConfig{Enabled: &enabled}
	tmpl := &store.Template{Name: "t", Config: &store.TemplateConfig{Telemetry: prev}}
	applyAgentConfigUpload(tmpl, []byte("telemetry: [unclosed"), prev)
	if tmpl.Config.Telemetry != prev {
		t.Errorf("expected telemetry to be kept, got %+v", tmpl.Config.Telemetry)
	}
}
