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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockADCSource implements TokenSource for testing.
type mockADCSource struct {
	audience string
	token    string
	expiry   time.Time
}

func (m *mockADCSource) Token() (string, error) {
	if m.token == "" {
		return "", fmt.Errorf("no token")
	}
	return m.token, nil
}

func (m *mockADCSource) SetToken(token string, expiry time.Time) {
	m.token = token
	m.expiry = expiry
}

func (m *mockADCSource) Expiry() time.Time {
	return m.expiry
}

func mockADCNew(audience string) (TokenSource, error) {
	return &mockADCSource{audience: audience, token: makeTestJWT(time.Now().Add(1 * time.Hour))}, nil
}

func mockADCNewFailing(audience string) (TokenSource, error) {
	return nil, fmt.Errorf("ADC not available")
}

func TestResolveBrokerTransport_NoConfig(t *testing.T) {
	t.Setenv(EnvTransportMode, "")
	t.Setenv(EnvTransportAudience, "")

	src, mode, err := ResolveBrokerTransport("", "", nil)
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
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	t.Setenv(EnvTransportMode, "")
	t.Setenv(EnvTransportAudience, "")
	t.Setenv(EnvMetadataMode, "")

	src, mode, err := ResolveBrokerTransport("iap", "test-audience.apps.googleusercontent.com", nil)
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
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	t.Setenv(EnvTransportMode, "cloudrun_invoker")
	t.Setenv(EnvTransportAudience, "env-audience")
	t.Setenv(EnvMetadataMode, "")

	src, mode, err := ResolveBrokerTransport("iap", "creds-audience", nil)
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
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	t.Setenv(EnvTransportMode, "iap")
	t.Setenv(EnvTransportAudience, "")
	t.Setenv(EnvMetadataMode, "")

	src, mode, err := ResolveBrokerTransport("cloudrun_invoker", "creds-audience", nil)
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
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	t.Setenv(EnvTransportMode, "")
	t.Setenv(EnvTransportAudience, "")
	t.Setenv(EnvMetadataMode, "")

	src, mode, err := ResolveBrokerTransport("", "audience-only", nil)
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
	t.Setenv(EnvTransportMode, "")
	t.Setenv(EnvTransportAudience, "")

	src, _, err := ResolveBrokerTransport("iap", "", nil)
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

func TestResolveBrokerTransport_MetadataPrecedence(t *testing.T) {
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	t.Setenv(EnvMetadataMode, "")
	t.Setenv(EnvTransportAudience, "https://audience.example.com")
	t.Setenv(EnvTransportMode, "")

	src, _, err := ResolveBrokerTransport("", "", mockADCNew)
	require.NoError(t, err)
	require.NotNil(t, src)
	_, ok := src.(*MetadataSource)
	assert.True(t, ok, "should return MetadataSource when on GCE")
}

func TestResolveBrokerTransport_ADCFallback(t *testing.T) {
	cleanup := overrideGCPDetection(false)
	defer cleanup()

	t.Setenv(EnvTransportAudience, "https://audience.example.com")
	t.Setenv(EnvTransportMode, "")

	src, _, err := ResolveBrokerTransport("", "", mockADCNew)
	require.NoError(t, err)
	require.NotNil(t, src)
	_, ok := src.(*mockADCSource)
	assert.True(t, ok, "should fall back to ADC when not on GCE")
}

func TestResolveBrokerTransport_ADCFallbackWhenMetadataBlocked(t *testing.T) {
	cleanup := overrideGCPDetection(true)
	defer cleanup()

	t.Setenv(EnvMetadataMode, "assign")
	t.Setenv(EnvTransportAudience, "https://audience.example.com")
	t.Setenv(EnvTransportMode, "")

	src, _, err := ResolveBrokerTransport("", "", mockADCNew)
	require.NoError(t, err)
	require.NotNil(t, src)
	_, ok := src.(*mockADCSource)
	assert.True(t, ok, "should fall back to ADC when SCION_METADATA_MODE is set")
}

func TestResolveBrokerTransport_NilADCConstructor(t *testing.T) {
	cleanup := overrideGCPDetection(false)
	defer cleanup()

	t.Setenv(EnvTransportAudience, "https://audience.example.com")
	t.Setenv(EnvTransportMode, "")

	src, _, err := ResolveBrokerTransport("", "", nil)
	require.NoError(t, err)
	assert.Nil(t, src, "should return nil when ADC constructor is nil and not on GCE")
}

func TestResolveBrokerTransport_ADCError(t *testing.T) {
	cleanup := overrideGCPDetection(false)
	defer cleanup()

	t.Setenv(EnvTransportAudience, "https://audience.example.com")
	t.Setenv(EnvTransportMode, "")

	_, _, err := ResolveBrokerTransport("", "", mockADCNewFailing)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ADC not available")
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
