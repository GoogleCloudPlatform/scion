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

package secret

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const testTelemetryTarget = "~/.scion/telemetry-gcp-credentials.json"

func TestHubSecretDeliverable(t *testing.T) {
	cases := []struct {
		name        string
		meta        SecretMeta
		deliverable bool
		visible     bool
	}{
		{"env always", SecretMeta{Name: "K", Scope: ScopeHub, SecretType: TypeEnvironment, InjectionMode: store.InjectionModeAlways}, true, true},
		{"untyped always", SecretMeta{Name: "K", Scope: ScopeHub, InjectionMode: store.InjectionModeAlways}, true, true},
		{"env as_needed", SecretMeta{Name: "K", Scope: ScopeHub, SecretType: TypeEnvironment, InjectionMode: store.InjectionModeAsNeeded}, false, false},
		{"env empty mode", SecretMeta{Name: "K", Scope: ScopeHub, SecretType: TypeEnvironment}, false, false},
		{"variable always", SecretMeta{Name: "K", Scope: ScopeHub, SecretType: TypeVariable, InjectionMode: store.InjectionModeAlways}, false, false},
		{"file always", SecretMeta{Name: "K", Scope: ScopeHub, SecretType: TypeFile, Target: "/etc/k", InjectionMode: store.InjectionModeAlways}, false, false},
		{"internal always", SecretMeta{Name: "K", Scope: ScopeHub, SecretType: store.SecretTypeInternal, InjectionMode: store.InjectionModeAlways}, false, false},
		{"telemetry file", SecretMeta{Name: telemetryCredentialFileName, Scope: ScopeHub, SecretType: TypeFile, Target: testTelemetryTarget}, true, false},
		{"telemetry file absolute target", SecretMeta{Name: telemetryCredentialFileName, Scope: ScopeHub, SecretType: TypeFile, Target: "/home/scion/.scion/telemetry-gcp-credentials.json"}, true, false},
		{"telemetry file uncleaned target", SecretMeta{Name: telemetryCredentialFileName, Scope: ScopeHub, SecretType: TypeFile, Target: "/home/scion/.scion/x/../telemetry-gcp-credentials.json"}, true, false},
		{"telemetry name other target", SecretMeta{Name: telemetryCredentialFileName, Scope: ScopeHub, SecretType: TypeFile, Target: "/etc/telemetry.json"}, false, false},
		{"telemetry name target without segment boundary", SecretMeta{Name: telemetryCredentialFileName, Scope: ScopeHub, SecretType: TypeFile, Target: "/tmp/x.scion/telemetry-gcp-credentials.json"}, false, false},
		{"telemetry name variable type", SecretMeta{Name: telemetryCredentialFileName, Scope: ScopeHub, SecretType: TypeVariable, Target: testTelemetryTarget}, false, false},
		{"other file name at telemetry target", SecretMeta{Name: "OTHER_FILE", Scope: ScopeHub, SecretType: TypeFile, Target: testTelemetryTarget}, false, false},
		{"telemetry file outside hub scope", SecretMeta{Name: telemetryCredentialFileName, Scope: ScopeProject, SecretType: TypeFile, Target: testTelemetryTarget}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HubSecretDeliverable(tc.meta); got != tc.deliverable {
				t.Errorf("HubSecretDeliverable = %v, want %v", got, tc.deliverable)
			}
			if got := HubSecretAgentVisible(tc.meta); got != tc.visible {
				t.Errorf("HubSecretAgentVisible = %v, want %v", got, tc.visible)
			}
		})
	}
}

// hubScopeResolveBackend is the subset of a backend the hub-scope delivery
// tests drive.
type hubScopeResolveBackend interface {
	Set(ctx context.Context, input *SetSecretInput) (bool, *SecretMeta, error)
	Resolve(ctx context.Context, userID, projectID, brokerID string, opts *ResolveOpts) ([]SecretWithValue, error)
}

// runHubScopeResolveCases pins which hub-scope secrets Resolve delivers:
// environment secrets in "always" mode and the telemetry credential file
// only. Other hub-scope rows are absent, so a broker-scope row of the same
// name is delivered, and project, user and broker scopes are unchanged.
func runHubScopeResolveCases(t *testing.T, newBackend func(t *testing.T) hubScopeResolveBackend) {
	t.Helper()
	ctx := context.Background()
	b := newBackend(t)
	hub := func(name, typ, target, mode string) *SetSecretInput {
		return &SetSecretInput{Name: name, Value: "hub-" + name, SecretType: typ, Target: target, Scope: ScopeHub, ScopeID: "test-hub-id", InjectionMode: mode}
	}
	for _, in := range []*SetSecretInput{
		hub("HUB_ENV_ALWAYS", TypeEnvironment, "", store.InjectionModeAlways),
		hub("HUB_ENV_AS_NEEDED", TypeEnvironment, "", store.InjectionModeAsNeeded),
		hub("HUB_VARIABLE", TypeVariable, "", store.InjectionModeAlways),
		hub("HUB_FILE", TypeFile, "/etc/hub-file", store.InjectionModeAlways),
		hub(telemetryCredentialFileName, TypeFile, testTelemetryTarget, ""),
		hub("HUB_OTHER_TELEMETRY_FILE", TypeFile, "/home/scion/.scion/other/telemetry-gcp-credentials.json", ""),
		hub("SHADOWED", TypeVariable, "", store.InjectionModeAlways),
		{Name: "SHADOWED", Value: "broker-SHADOWED", SecretType: TypeVariable, Scope: ScopeRuntimeBroker, ScopeID: "broker-1"},
		{Name: "BROKER_FILE", Value: "broker-file", SecretType: TypeFile, Target: "/etc/broker-file", Scope: ScopeRuntimeBroker, ScopeID: "broker-1"},
		{Name: "PROJECT_ENV_AS_NEEDED", Value: "project-env", SecretType: TypeEnvironment, Scope: ScopeProject, ScopeID: "project-1", InjectionMode: store.InjectionModeAsNeeded},
		{Name: "PROJECT_VARIABLE", Value: "project-variable", SecretType: TypeVariable, Scope: ScopeProject, ScopeID: "project-1"},
		{Name: "PROJECT_FILE", Value: "project-file", SecretType: TypeFile, Target: "/etc/project-file", Scope: ScopeProject, ScopeID: "project-1"},
		{Name: "USER_FILE", Value: "user-file", SecretType: TypeFile, Target: "/etc/user-file", Scope: ScopeUser, ScopeID: "user-1"},
	} {
		if _, _, err := b.Set(ctx, in); err != nil {
			t.Fatalf("Set %s/%s: %v", in.Scope, in.Name, err)
		}
	}

	resolved, err := b.Resolve(ctx, "user-1", "project-1", "broker-1", nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got := make(map[string]SecretWithValue, len(resolved))
	for _, sv := range resolved {
		got[sv.Name] = sv
	}

	for _, name := range []string{"HUB_ENV_ALWAYS", telemetryCredentialFileName} {
		if sv, ok := got[name]; !ok || sv.Scope != ScopeHub || sv.Value != "hub-"+name {
			t.Errorf("%s: got %+v (present %v), want the hub row delivered", name, sv.SecretMeta, ok)
		}
	}
	for _, name := range []string{"HUB_ENV_AS_NEEDED", "HUB_VARIABLE", "HUB_FILE", "HUB_OTHER_TELEMETRY_FILE"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s: delivered, want the hub row excluded", name)
		}
	}
	if sv := got["SHADOWED"]; sv.Scope != ScopeRuntimeBroker || sv.Value != "broker-SHADOWED" {
		t.Errorf("SHADOWED: got scope %q value %q, want the broker row once the hub row is excluded", sv.Scope, sv.Value)
	}
	for name, scope := range map[string]string{
		"BROKER_FILE":           ScopeRuntimeBroker,
		"PROJECT_ENV_AS_NEEDED": ScopeProject,
		"PROJECT_VARIABLE":      ScopeProject,
		"PROJECT_FILE":          ScopeProject,
		"USER_FILE":             ScopeUser,
	} {
		if sv, ok := got[name]; !ok || sv.Scope != scope {
			t.Errorf("%s: got %+v (present %v), want the %s row unchanged", name, sv.SecretMeta, ok, scope)
		}
	}
	if len(got) != 8 {
		t.Errorf("resolved %d secrets, want 8: %v", len(got), got)
	}
}

func TestLocalBackend_ResolveHubScopeDelivery(t *testing.T) {
	runHubScopeResolveCases(t, func(t *testing.T) hubScopeResolveBackend {
		b, _ := createTestBackend(t)
		return b
	})
}

func TestGCPBackend_ResolveHubScopeDelivery(t *testing.T) {
	runHubScopeResolveCases(t, func(t *testing.T) hubScopeResolveBackend {
		b, _ := createTestGCPBackend(t)
		return b
	})
}
