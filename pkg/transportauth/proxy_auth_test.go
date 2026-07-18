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

package transportauth

import (
	"os"
	"testing"
)

func TestFromEnv_ProxyAuthState_WithTransportToken(t *testing.T) {
	t.Setenv(EnvTransportToken, "injected-token")

	src, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src == nil {
		t.Fatal("expected non-nil source when SCION_TRANSPORT_TOKEN is set")
	}

	tok, err := src.Token()
	if err != nil {
		t.Fatalf("Token() failed: %v", err)
	}
	if tok != "injected-token" {
		t.Errorf("expected %q, got %q", "injected-token", tok)
	}
}

func TestFromEnv_NoTransportAuth(t *testing.T) {
	t.Setenv(EnvTransportToken, "")
	t.Setenv(EnvTransportAudience, "")
	t.Setenv(EnvHubOIDCAudience, "")

	origOnGCE := IsOnGCEFunc
	defer func() { IsOnGCEFunc = origOnGCE }()
	IsOnGCEFunc = func() bool { return false }

	src, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src != nil {
		t.Error("expected nil source when no transport auth configured")
	}
}

func TestFromEnv_MetadataOnGCE(t *testing.T) {
	t.Setenv(EnvTransportToken, "")
	t.Setenv(EnvMetadataMode, "")
	t.Setenv(EnvTransportAudience, "test-audience-for-gce")

	origOnGCE := IsOnGCEFunc
	defer func() { IsOnGCEFunc = origOnGCE }()
	IsOnGCEFunc = func() bool { return true }

	src, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src == nil {
		t.Fatal("expected MetadataSource on GCE with audience set")
	}
	ms, ok := src.(*MetadataSource)
	if !ok {
		t.Fatalf("expected *MetadataSource, got %T", src)
	}
	if ms.Audience() != "test-audience-for-gce" {
		t.Errorf("audience: expected %q, got %q", "test-audience-for-gce", ms.Audience())
	}
}

func TestRegistrationPersistence_TransportFields(t *testing.T) {
	t.Setenv(EnvTransportMode, "")
	t.Setenv(EnvTransportAudience, "")

	// Simulate what runBrokerRegister does: resolve from flags then env
	flagMode := "iap"
	flagAudience := "12345.apps.googleusercontent.com"

	transportMode := flagMode
	if transportMode == "" {
		transportMode = os.Getenv(EnvTransportMode)
	}
	transportAudience := flagAudience
	if transportAudience == "" {
		transportAudience = os.Getenv(EnvTransportAudience)
	}

	if transportMode != "iap" {
		t.Errorf("expected mode %q, got %q", "iap", transportMode)
	}
	if transportAudience != "12345.apps.googleusercontent.com" {
		t.Errorf("expected audience %q, got %q", "12345.apps.googleusercontent.com", transportAudience)
	}
}

func TestRegistrationPersistence_FallsBackToEnv(t *testing.T) {
	t.Setenv(EnvTransportMode, "cloudrun_invoker")
	t.Setenv(EnvTransportAudience, "env-audience")

	// Simulate empty flags
	flagMode := ""
	flagAudience := ""

	transportMode := flagMode
	if transportMode == "" {
		transportMode = os.Getenv(EnvTransportMode)
	}
	transportAudience := flagAudience
	if transportAudience == "" {
		transportAudience = os.Getenv(EnvTransportAudience)
	}

	if transportMode != "cloudrun_invoker" {
		t.Errorf("expected mode %q, got %q", "cloudrun_invoker", transportMode)
	}
	if transportAudience != "env-audience" {
		t.Errorf("expected audience %q, got %q", "env-audience", transportAudience)
	}
}
