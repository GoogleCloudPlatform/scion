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
	"strings"
	"testing"
)

func TestResolveBrokerTransport_NoConfig(t *testing.T) {
	os.Unsetenv(EnvTransportMode)
	os.Unsetenv(EnvTransportAudience)

	src, mode, err := ResolveBrokerTransport("", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src != nil {
		t.Error("expected nil source when no config present")
	}
	if mode != HeaderAuthorization {
		t.Errorf("expected HeaderAuthorization, got %v", mode)
	}
}

func TestResolveBrokerTransport_FromCredentials(t *testing.T) {
	os.Unsetenv(EnvTransportMode)
	os.Unsetenv(EnvTransportAudience)

	src, mode, err := ResolveBrokerTransport("iap", "test-audience.apps.googleusercontent.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src == nil {
		t.Fatal("expected non-nil source")
	}
	if mode != HeaderProxyAuthorization {
		t.Errorf("expected HeaderProxyAuthorization, got %v", mode)
	}

	ms, ok := src.(*MetadataSource)
	if !ok {
		t.Fatalf("expected *MetadataSource, got %T", src)
	}
	if ms.Audience() != "test-audience.apps.googleusercontent.com" {
		t.Errorf("audience: expected %q, got %q", "test-audience.apps.googleusercontent.com", ms.Audience())
	}
}

func TestResolveBrokerTransport_EnvOverridesCreds(t *testing.T) {
	t.Setenv(EnvTransportMode, "cloudrun_invoker")
	t.Setenv(EnvTransportAudience, "env-audience")

	src, mode, err := ResolveBrokerTransport("iap", "creds-audience")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src == nil {
		t.Fatal("expected non-nil source")
	}
	if mode != HeaderServerlessAuthorization {
		t.Errorf("expected HeaderServerlessAuthorization, got %v", mode)
	}

	ms := src.(*MetadataSource)
	if ms.Audience() != "env-audience" {
		t.Errorf("audience: expected %q, got %q", "env-audience", ms.Audience())
	}
}

func TestResolveBrokerTransport_PartialEnvOverride(t *testing.T) {
	t.Setenv(EnvTransportMode, "iap")
	os.Unsetenv(EnvTransportAudience)

	src, mode, err := ResolveBrokerTransport("cloudrun_invoker", "creds-audience")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src == nil {
		t.Fatal("expected non-nil source")
	}
	// Mode from env overrides creds
	if mode != HeaderProxyAuthorization {
		t.Errorf("expected HeaderProxyAuthorization, got %v", mode)
	}
	// Audience falls back to creds
	ms := src.(*MetadataSource)
	if ms.Audience() != "creds-audience" {
		t.Errorf("audience: expected %q, got %q", "creds-audience", ms.Audience())
	}
}

func TestResolveBrokerTransport_AudienceOnlyCreatesSource(t *testing.T) {
	os.Unsetenv(EnvTransportMode)
	os.Unsetenv(EnvTransportAudience)

	src, mode, err := ResolveBrokerTransport("", "audience-only")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src == nil {
		t.Fatal("expected non-nil source when audience is set")
	}
	if mode != HeaderAuthorization {
		t.Errorf("expected HeaderAuthorization (default), got %v", mode)
	}
}

func TestResolveBrokerTransport_ModeWithoutAudience(t *testing.T) {
	os.Unsetenv(EnvTransportMode)
	os.Unsetenv(EnvTransportAudience)

	src, _, err := ResolveBrokerTransport("iap", "")
	if err == nil {
		t.Fatal("expected error when mode is set but audience is empty")
	}
	if src != nil {
		t.Error("expected nil source on error")
	}
	if !strings.Contains(err.Error(), "audience is required") {
		t.Errorf("expected error to contain 'audience is required', got: %v", err)
	}
}

func TestModeFromString(t *testing.T) {
	tests := []struct {
		input    string
		expected HeaderMode
	}{
		{"iap", HeaderProxyAuthorization},
		{"cloudrun_invoker", HeaderServerlessAuthorization},
		{"", HeaderAuthorization},
		{"unknown", HeaderAuthorization},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := ModeFromString(tc.input)
			if result != tc.expected {
				t.Errorf("ModeFromString(%q) = %v, want %v", tc.input, result, tc.expected)
			}
		})
	}
}
