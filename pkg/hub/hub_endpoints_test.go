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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func resolveForTest(t *testing.T, base HubEndpoints, dbValue string) (HubEndpoints, string) {
	t.Helper()
	var buf bytes.Buffer
	got := ResolveHubEndpoints(base, dbValue, slog.New(slog.NewTextHandler(&buf, nil)))
	return got, buf.String()
}

// TestResolveHubEndpoints_Precedence covers the precedence of
// server.hub.public_url at startup: an operator pin (env or flag) first,
// then a valid database value, then settings.yaml and the existing fallbacks
// (ptone/scion#3905).
func TestResolveHubEndpoints_Precedence(t *testing.T) {
	const (
		fileURL = "https://file.example.com"
		envURL  = "https://env.example.com"
		dbURL   = "https://db.example.com"
	)
	tests := []struct {
		name       string
		base       HubEndpoints
		dbValue    string
		wantPublic string
		wantSource string
	}{
		{"file only", HubEndpoints{Operator: fileURL, OperatorSource: "file"}, "", fileURL, "file"},
		{"env only", HubEndpoints{Operator: envURL, OperatorSource: "env", Pin: "SCION_SERVER_HUB_ENDPOINT"}, "", envURL, "env"},
		{"database only", HubEndpoints{Operator: "http://localhost:8080", OperatorSource: "localhost"}, dbURL, dbURL, HubEndpointSourceDatabase},
		{"database over file", HubEndpoints{Operator: fileURL, OperatorSource: "file"}, dbURL, dbURL, HubEndpointSourceDatabase},
		{"env over database", HubEndpoints{Operator: envURL, OperatorSource: "env", Pin: "SCION_SERVER_HUB_ENDPOINT"}, dbURL, envURL, "env"},
		{"env over database and file", HubEndpoints{Operator: envURL, OperatorSource: "env", Pin: "SCION_SERVER_HUB_ENDPOINT"}, dbURL, envURL, "env"},
		{"flag over database", HubEndpoints{Operator: "https://flag.example.com", OperatorSource: "flag", Pin: "--base-url"}, dbURL, "https://flag.example.com", "flag"},
		{"base URL env pins a file value", HubEndpoints{Operator: fileURL, OperatorSource: "file", Pin: "SCION_SERVER_BASE_URL"}, dbURL, fileURL, "file"},
		{"database value normalized", HubEndpoints{Operator: fileURL, OperatorSource: "file"}, "HTTPS://db.example.com/", dbURL, HubEndpointSourceDatabase},
		{"database value equal to operator", HubEndpoints{Operator: "http://hub.internal:8080", OperatorSource: "file"}, "http://hub.internal:8080/", "http://hub.internal:8080", "file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := resolveForTest(t, tt.base, tt.dbValue)
			if got.Public != tt.wantPublic || got.PublicSource != tt.wantSource {
				t.Errorf("Public = %q (source %q), want %q (source %q)", got.Public, got.PublicSource, tt.wantPublic, tt.wantSource)
			}
			if got.Operator != tt.base.Operator {
				t.Errorf("Operator = %q, want it unchanged (%q)", got.Operator, tt.base.Operator)
			}
		})
	}
}

// TestResolveHubEndpoints_InvalidDatabaseValueFallsBack checks that an
// invalid database value is not used: the operator endpoint applies and an
// error is logged.
func TestResolveHubEndpoints_InvalidDatabaseValueFallsBack(t *testing.T) {
	for _, bad := range []string{
		"http://db.example.com",
		"https://user:pw@db.example.com",
		"https://db.example.com/#frag",
		"https://db.example.com/?q=1",
		"https://db.example.com/sub",
		"db.example.com",
	} {
		t.Run(bad, func(t *testing.T) {
			got, logged := resolveForTest(t, HubEndpoints{Operator: "https://file.example.com", OperatorSource: "file"}, bad)
			if got.Public != "https://file.example.com" || got.PublicSource != "file" {
				t.Errorf("Public = %q (source %q), want the file value", got.Public, got.PublicSource)
			}
			if !strings.Contains(logged, "level=ERROR") || !strings.Contains(logged, "invalid and is not used") {
				t.Errorf("expected an error log for the invalid database value, got: %s", logged)
			}
		})
	}
}

// TestResolveHubEndpoints_LogLines checks the startup log: one line naming
// the source and value of each endpoint, and a line when the database value
// overrides a settings.yaml value.
func TestResolveHubEndpoints_LogLines(t *testing.T) {
	_, logged := resolveForTest(t, HubEndpoints{Operator: "https://file.example.com", OperatorSource: "file"}, "https://db.example.com")
	for _, want := range []string{
		`msg="Hub operator endpoint (environment, flags and settings.yaml)" source=file endpoint=https://file.example.com`,
		`msg="Hub endpoint in effect" source=database endpoint=https://db.example.com`,
		"overrides the settings.yaml value",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log does not contain %q:\n%s", want, logged)
		}
	}
	if n := strings.Count(logged, `msg="Hub endpoint in effect"`); n != 1 {
		t.Errorf("logged the endpoint in effect %d times, want once", n)
	}

	_, logged = resolveForTest(t, HubEndpoints{Operator: "https://env.example.com", OperatorSource: "env", Pin: "SCION_SERVER_HUB_ENDPOINT"}, "https://db.example.com")
	if !strings.Contains(logged, `source=env endpoint=https://env.example.com pinned_by=SCION_SERVER_HUB_ENDPOINT`) {
		t.Errorf("pinned log line missing, got:\n%s", logged)
	}
	if strings.Contains(logged, "overrides the settings.yaml value") {
		t.Errorf("a pinned endpoint must not log a database override, got:\n%s", logged)
	}

	_, logged = resolveForTest(t, HubEndpoints{Operator: "http://localhost:8080", OperatorSource: "localhost"}, "https://db.example.com")
	if strings.Contains(logged, "overrides the settings.yaml value") {
		t.Errorf("no settings.yaml value was present, so no override line is expected, got:\n%s", logged)
	}
}

// TestHubEndpointUses checks the per-use table: every use has an entry, and
// every use, including the OIDC issuer and transport audience defaults,
// gets the endpoint in effect (ptone/scion#3905).
func TestHubEndpointUses(t *testing.T) {
	uses := []HubEndpointUse{
		HubEndpointForAgents,
		HubEndpointForInviteLinks,
		HubEndpointForChatBridge,
		HubEndpointForPluginBrokers,
		HubEndpointForProjectSettings,
		HubEndpointForJoinResponse,
		HubEndpointForOIDCIssuer,
		HubEndpointForTransportAudience,
		HubEndpointForConduitCheck,
	}
	if len(uses) != len(hubEndpointUses) {
		t.Errorf("hubEndpointUses has %d entries, this test lists %d uses", len(hubEndpointUses), len(uses))
	}
	e := HubEndpoints{Operator: "https://operator.example.com", Public: "https://db.example.com"}
	for _, use := range uses {
		if _, ok := hubEndpointUses[use]; !ok {
			t.Errorf("use %q has no entry in hubEndpointUses", use)
		}
		if got := e.For(use); got != e.Public {
			t.Errorf("For(%q) = %q, want the endpoint in effect %q", use, got, e.Public)
		}
	}
	if got := (HubEndpoints{Operator: "https://operator.example.com"}).For(HubEndpointForAgents); got != "https://operator.example.com" {
		t.Errorf("For with no public endpoint = %q, want the operator endpoint", got)
	}
}

// TestHubEndpointsFromConfig checks that a server built without resolved
// endpoints uses HubEndpoint for every use, and one with them uses the
// endpoint in effect.
func TestHubEndpointsFromConfig(t *testing.T) {
	srv := &Server{config: ServerConfig{HubEndpoint: "https://operator.example.com"}}
	if got := srv.HubEndpointFor(HubEndpointForInviteLinks); got != "https://operator.example.com" {
		t.Errorf("HubEndpointFor without resolved endpoints = %q, want HubEndpoint", got)
	}
	srv = &Server{config: ServerConfig{
		HubEndpoint:  "https://operator.example.com",
		HubEndpoints: &HubEndpoints{Operator: "https://operator.example.com", Public: "https://db.example.com"},
	}}
	for _, use := range []HubEndpointUse{HubEndpointForInviteLinks, HubEndpointForAgents, HubEndpointForOIDCIssuer} {
		if got := srv.HubEndpointFor(use); got != "https://db.example.com" {
			t.Errorf("HubEndpointFor(%q) = %q, want the database value", use, got)
		}
	}
}

// TestStoredPublicURL checks which settings rows count as a saved public URL
// at startup: an admin-saved (managed) row does; a seeded row does not, so a
// stale seeded value from an earlier settings.yaml cannot override the
// current file value.
func TestStoredPublicURL(t *testing.T) {
	ctx := context.Background()
	doc := json.RawMessage(`{"public_url":"https://saved.example.com"}`)

	t.Run("no row", func(t *testing.T) {
		got, err := StoredPublicURL(ctx, newFakeHubSettingStore())
		if err != nil || got != "" {
			t.Errorf("StoredPublicURL = %q, %v; want \"\", nil", got, err)
		}
	})
	t.Run("managed row", func(t *testing.T) {
		st := newFakeHubSettingStore()
		if _, err := st.UpsertHubSetting(ctx, "endpoints", doc, "admin@example.com", -1, "managed"); err != nil {
			t.Fatal(err)
		}
		got, err := StoredPublicURL(ctx, st)
		if err != nil || got != "https://saved.example.com" {
			t.Errorf("StoredPublicURL = %q, %v; want the saved value", got, err)
		}
	})
	t.Run("row awaiting origin backfill", func(t *testing.T) {
		st := newFakeHubSettingStore()
		if _, err := st.UpsertHubSetting(ctx, "endpoints", doc, "admin@example.com", -1, "seeded"); err != nil {
			t.Fatal(err)
		}
		got, err := StoredPublicURL(ctx, st)
		if err != nil || got != "https://saved.example.com" {
			t.Errorf("StoredPublicURL = %q, %v; want the saved value (BackfillOrigin makes this row managed)", got, err)
		}
	})
	t.Run("stale seeded row does not override a changed file value", func(t *testing.T) {
		st := newFakeHubSettingStore()
		stale := json.RawMessage(`{"public_url":"https://old-file.example.com"}`)
		if _, err := st.UpsertHubSetting(ctx, "endpoints", stale, "seed", -1, "seeded"); err != nil {
			t.Fatal(err)
		}
		got, err := StoredPublicURL(ctx, st)
		if err != nil || got != "" {
			t.Fatalf("StoredPublicURL = %q, %v; want a seeded row ignored", got, err)
		}
		resolved, _ := resolveForTest(t, HubEndpoints{Operator: "https://new-file.example.com", OperatorSource: "file"}, got)
		if resolved.Public != "https://new-file.example.com" || resolved.PublicSource != "file" {
			t.Errorf("Public = %q (source %q), want the current file value", resolved.Public, resolved.PublicSource)
		}
	})
	t.Run("malformed row", func(t *testing.T) {
		st := newFakeHubSettingStore()
		if _, err := st.UpsertHubSetting(ctx, "endpoints", json.RawMessage(`{"public_url":1}`), "admin@example.com", -1, "managed"); err != nil {
			t.Fatal(err)
		}
		got, err := StoredPublicURL(ctx, st)
		if err != nil || got != "" {
			t.Errorf("StoredPublicURL = %q, %v; want a malformed row ignored", got, err)
		}
	})
}
