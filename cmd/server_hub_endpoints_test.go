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

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func setWebBaseURL(t *testing.T, v string) {
	t.Helper()
	saved := webBaseURL
	webBaseURL = v
	t.Cleanup(func() { webBaseURL = saved })
}

// TestHubEndpointPin checks that every operator source pins the hub
// endpoint: SCION_SERVER_HUB_ENDPOINT, --base-url and SCION_SERVER_BASE_URL
// (ptone/scion#3905).
func TestHubEndpointPin(t *testing.T) {
	t.Setenv("SCION_SERVER_HUB_ENDPOINT", "")
	t.Setenv("SCION_SERVER_BASE_URL", "")
	setWebBaseURL(t, "")
	if got := hubEndpointPin(); got != "" {
		t.Errorf("no pin set: hubEndpointPin() = %q, want \"\"", got)
	}

	t.Setenv("SCION_SERVER_BASE_URL", "https://base.example.com")
	if got := hubEndpointPin(); got != "SCION_SERVER_BASE_URL" {
		t.Errorf("hubEndpointPin() = %q, want SCION_SERVER_BASE_URL", got)
	}
	setWebBaseURL(t, "https://flag.example.com")
	if got := hubEndpointPin(); got != "--base-url" {
		t.Errorf("hubEndpointPin() = %q, want --base-url", got)
	}
	t.Setenv("SCION_SERVER_HUB_ENDPOINT", "https://env.example.com")
	if got := hubEndpointPin(); got != "SCION_SERVER_HUB_ENDPOINT" {
		t.Errorf("hubEndpointPin() = %q, want SCION_SERVER_HUB_ENDPOINT", got)
	}
}

// TestOperatorHubEndpointSourceLabel checks the source names logged and
// shown in the admin view for the operator endpoint.
func TestOperatorHubEndpointSourceLabel(t *testing.T) {
	t.Setenv("SCION_SERVER_HUB_ENDPOINT", "")
	tests := []struct {
		src  hubEndpointSource
		want string
	}{
		{hubEndpointSourceConfig, "file"},
		{hubEndpointSourceBaseURLFlag, "flag"},
		{hubEndpointSourceBaseURLEnv, "env"},
		{hubEndpointSourceSettings, "settings"},
		{hubEndpointSourceBrokerSettings, "settings"},
		{hubEndpointSourceIAPAudience, "iap-audience"},
		{hubEndpointSourceLocalhost, "localhost"},
	}
	for _, tt := range tests {
		if got := (operatorHubEndpoint{Source: tt.src}).sourceLabel(); got != tt.want {
			t.Errorf("sourceLabel(%q) = %q, want %q", tt.src, got, tt.want)
		}
	}
	t.Setenv("SCION_SERVER_HUB_ENDPOINT", "https://env.example.com")
	if got := (operatorHubEndpoint{Source: hubEndpointSourceConfig}).sourceLabel(); got != "env" {
		t.Errorf("config source with SCION_SERVER_HUB_ENDPOINT set: sourceLabel() = %q, want env", got)
	}
}

// endpointsTestStore is a store whose only working method is the
// settings-row read resolveHubEndpoints makes.
type endpointsTestStore struct {
	store.Store
	row *store.HubSetting
}

func newEndpointsTestStore(t *testing.T, doc, updatedBy, origin string) *endpointsTestStore {
	t.Helper()
	return &endpointsTestStore{row: &store.HubSetting{
		Section: "endpoints", Value: json.RawMessage(doc), Revision: 1, UpdatedBy: updatedBy, Origin: origin,
	}}
}

func (s *endpointsTestStore) GetHubSetting(_ context.Context, section string) (*store.HubSetting, error) {
	if section != "endpoints" || s.row == nil {
		return nil, store.ErrNotFound
	}
	return s.row, nil
}

func (s *endpointsTestStore) ListHubSettings(context.Context) ([]store.HubSetting, error) {
	return nil, errors.New("not implemented")
}

func (s *endpointsTestStore) UpsertHubSetting(context.Context, string, json.RawMessage, string, int64, string) (*store.HubSetting, error) {
	return nil, errors.New("not implemented")
}

func (s *endpointsTestStore) DeleteHubSetting(context.Context, string) error {
	return errors.New("not implemented")
}

func (s *endpointsTestStore) BackfillOrigin(context.Context) error {
	return errors.New("not implemented")
}

// TestResolveHubEndpoints_StartupSplit checks the startup resolution end to
// end: a saved public URL becomes the endpoint for agents, invites and the
// transport audience; a pin keeps the operator endpoint.
func TestResolveHubEndpoints_StartupSplit(t *testing.T) {
	savedEnableHub := enableHub
	enableHub = true
	t.Cleanup(func() { enableHub = savedEnableHub })
	setWebBaseURL(t, "")
	t.Setenv("SCION_SERVER_BASE_URL", "")
	t.Setenv("SCION_SERVER_HUB_ENDPOINT", "")

	st := newEndpointsTestStore(t, `{"public_url":"https://db.example.com"}`, "admin@example.com", "managed")
	op := operatorHubEndpoint{URL: "https://file.example.com", Source: hubEndpointSourceConfig}

	e, err := resolveHubEndpoints(context.Background(), st, op)
	if err != nil {
		t.Fatalf("resolveHubEndpoints: %v", err)
	}
	for _, use := range []hub.HubEndpointUse{hub.HubEndpointForAgents, hub.HubEndpointForInviteLinks, hub.HubEndpointForTransportAudience, hub.HubEndpointForOIDCIssuer} {
		if got := e.For(use); got != "https://db.example.com" {
			t.Errorf("For(%q) = %q, want the saved public URL", use, got)
		}
	}
	if ep, src := effectiveHubEndpoint(e, op.Source); ep != "https://db.example.com" || src != hubEndpointSourceConfig {
		t.Errorf("effectiveHubEndpoint = %q, %q; want the saved public URL as explicitly configured", ep, src)
	}
	if got := resolveTransportAudience("", "cloudrun_invoker", e.For(hub.HubEndpointForTransportAudience)); got != "https://db.example.com" {
		t.Errorf("transport audience default = %q, want the saved public URL", got)
	}

	// A pin keeps the operator endpoint everywhere.
	t.Setenv("SCION_SERVER_HUB_ENDPOINT", "https://file.example.com")
	e, err = resolveHubEndpoints(context.Background(), st, op)
	if err != nil {
		t.Fatalf("resolveHubEndpoints: %v", err)
	}
	if got := e.For(hub.HubEndpointForAgents); got != "https://file.example.com" {
		t.Errorf("pinned: For(agents) = %q, want the operator endpoint", got)
	}
	if e.Pin != "SCION_SERVER_HUB_ENDPOINT" || e.PublicSource != "env" {
		t.Errorf("pinned: Pin = %q, PublicSource = %q; want SCION_SERVER_HUB_ENDPOINT, env", e.Pin, e.PublicSource)
	}

	// A broker-only process never reads the database value.
	enableHub = false
	t.Setenv("SCION_SERVER_HUB_ENDPOINT", "")
	e, err = resolveHubEndpoints(context.Background(), st, op)
	if err != nil {
		t.Fatalf("resolveHubEndpoints: %v", err)
	}
	if got := e.For(hub.HubEndpointForAgents); got != "https://file.example.com" {
		t.Errorf("broker-only: For(agents) = %q, want the operator endpoint", got)
	}
}
