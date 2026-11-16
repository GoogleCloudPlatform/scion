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

package runtimebroker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestCreateAgentRequest_HomeFilesJSONRoundTrip(t *testing.T) {
	t.Run("non-empty round-trips base64 on wire", func(t *testing.T) {
		orig := CreateAgentRequest{
			Name: "trigger-agent",
			HomeFiles: []api.HomeFile{
				{
					Path:    ".scion/trigger-event.json",
					Mode:    0o600,
					Content: []byte("{\"event_id\":\"ev-1\"}\n\x00"),
				},
			},
		}
		raw, err := json.Marshal(orig)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		if !strings.Contains(string(raw), `"homeFiles"`) {
			t.Fatalf("expected homeFiles field on wire, got %s", raw)
		}
		var got CreateAgentRequest
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		if len(got.HomeFiles) != 1 {
			t.Fatalf("len(HomeFiles) = %d, want 1", len(got.HomeFiles))
		}
		if got.HomeFiles[0].Path != orig.HomeFiles[0].Path ||
			got.HomeFiles[0].Mode != orig.HomeFiles[0].Mode ||
			!bytes.Equal(got.HomeFiles[0].Content, orig.HomeFiles[0].Content) {
			t.Fatalf("round-trip mismatch: got %+v, want %+v", got.HomeFiles[0], orig.HomeFiles[0])
		}
	})

	t.Run("omitted when empty", func(t *testing.T) {
		raw, err := json.Marshal(CreateAgentRequest{Name: "plain-agent"})
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		if strings.Contains(string(raw), "homeFiles") {
			t.Fatalf("expected homeFiles to be omitted when empty, got %s", raw)
		}
	})
}

func TestCreateAgent_HomeFilesValidationAndThreading(t *testing.T) {
	const canary = "BROKER_SECRET_CONTENT_CANARY_999"

	t.Run("refuses invalid HomeFiles before provisioning", func(t *testing.T) {
		cases := []struct {
			name  string
			files []api.HomeFile
			repro bool
		}{
			{
				name: "bad path outside .scion",
				files: []api.HomeFile{
					{Path: ".ssh/authorized_keys", Mode: 0o600, Content: []byte(canary)},
				},
			},
			{
				name: "dot-dot traversal",
				files: []api.HomeFile{
					{Path: ".scion/../escape", Mode: 0o600, Content: []byte(canary)},
				},
			},
			{
				name: "bad mode 0700",
				files: []api.HomeFile{
					{Path: ".scion/trigger-event.json", Mode: 0o700, Content: []byte(canary)},
				},
			},
			{
				name: "non-empty HomeFiles on reprovision refused",
				files: []api.HomeFile{
					{Path: ".scion/trigger-event.json", Mode: 0o600, Content: []byte(canary)},
				},
				repro: true,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				srv, mgr := newTestServerWithProvisionCapture()

				reqBody := CreateAgentRequest{
					Name:          "bad-homefiles-agent",
					ID:            "agent-bad-hf",
					Slug:          "bad-homefiles-agent",
					ProvisionOnly: tc.repro,
					Reprovision:   tc.repro,
					Config:        &CreateAgentConfig{Template: "claude"},
					HomeFiles:     tc.files,
				}
				raw, err := json.Marshal(reqBody)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(raw))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()

				srv.Handler().ServeHTTP(w, req)

				if w.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
				}
				if mgr.startCalled || mgr.provisionCalled || mgr.reprovisionCalled {
					t.Fatalf("expected no manager calls when HomeFiles validation fails (start=%v provision=%v reprovision=%v)",
						mgr.startCalled, mgr.provisionCalled, mgr.reprovisionCalled)
				}
				if strings.Contains(w.Body.String(), canary) {
					t.Errorf("error response leaked HomeFile content: %s", w.Body.String())
				}
			})
		}
	})

	t.Run("threads valid HomeFiles into StartOptions on create and provisionOnly", func(t *testing.T) {
		wantFiles := []api.HomeFile{
			{Path: ".scion/trigger-event.json", Mode: 0o600, Content: []byte(`{"fire_id":"f-1"}`)},
		}

		for _, provisionOnly := range []bool{false, true} {
			name := "full-create"
			if provisionOnly {
				name = "provision-only"
			}
			t.Run(name, func(t *testing.T) {
				srv, mgr := newTestServerWithProvisionCapture()
				reqBody := CreateAgentRequest{
					Name:          "good-hf-agent",
					ID:            "agent-good-hf",
					Slug:          "good-hf-agent",
					ProvisionOnly: provisionOnly,
					Config:        &CreateAgentConfig{Template: "claude"},
					HomeFiles:     wantFiles,
				}
				raw, err := json.Marshal(reqBody)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(raw))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()

				srv.Handler().ServeHTTP(w, req)

				if w.Code != http.StatusCreated {
					t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
				}
				if !mgr.lastOpts.FreshProvision {
					t.Fatal("expected FreshProvision=true")
				}
				if len(mgr.lastOpts.HomeFiles) != 1 ||
					mgr.lastOpts.HomeFiles[0].Path != wantFiles[0].Path ||
					mgr.lastOpts.HomeFiles[0].Mode != wantFiles[0].Mode ||
					!bytes.Equal(mgr.lastOpts.HomeFiles[0].Content, wantFiles[0].Content) {
					t.Fatalf("lastOpts.HomeFiles = %+v, want %+v", mgr.lastOpts.HomeFiles, wantFiles)
				}
			})
		}
	})

	t.Run("buildStartContext drops HomeFiles on non-create operations", func(t *testing.T) {
		srv, _ := newTestServerWithProvisionCapture()
		for _, op := range []startOperation{opHTTPStart, opHTTPRestart} {
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:        "resume-agent",
				ProjectPath: t.TempDir(),
				NoAuth:      true,
				Operation:   op,
				HomeFiles: []api.HomeFile{
					{Path: ".scion/trigger-event.json", Mode: 0o600, Content: []byte("ignored")},
				},
			})
			if err != nil {
				t.Fatalf("buildStartContext(%s): %v", op, err)
			}
			if len(sc.Opts.HomeFiles) != 0 {
				t.Errorf("op %s: expected Opts.HomeFiles to be empty, got %+v", op, sc.Opts.HomeFiles)
			}
		}
	})
}
