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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsSupportedIAPAudience(t *testing.T) {
	tests := []struct {
		name     string
		audience string
		want     bool
	}{
		{
			name:     "cloud run format",
			audience: "/projects/123/locations/us-central1/services/my-svc",
			want:     true,
		},
		{
			name:     "GCLB backend-service format",
			audience: "/projects/123/global/backendServices/456",
			want:     true,
		},
		{
			name:     "GCLB with alphanumeric id",
			audience: "/projects/999/global/backendServices/my-backend",
			want:     true,
		},
		{
			name:     "malformed path",
			audience: "/projects/123/foo/bar",
			want:     false,
		},
		{
			name:     "empty string",
			audience: "",
			want:     false,
		},
		{
			name:     "random string",
			audience: "not-a-valid-audience",
			want:     false,
		},
		{
			name:     "cloud run missing service name",
			audience: "/projects/123/locations/us-central1/services/",
			want:     false,
		},
		{
			name:     "GCLB missing id",
			audience: "/projects/123/global/backendServices/",
			want:     false,
		},
		{
			name:     "too many segments",
			audience: "/projects/123/global/backendServices/456/extra",
			want:     false,
		},
		{
			name:     "cloud run with trailing slash stripped",
			audience: "/projects/123/locations/us-central1/services/my-svc/",
			want:     false, // trailing slash produces empty last part
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSupportedIAPAudience(tt.audience)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIapAudienceToCloudRunURL(t *testing.T) {
	tests := []struct {
		name     string
		audience string
		want     string
	}{
		{
			name:     "valid cloud run audience",
			audience: "/projects/123456/locations/us-central1/services/my-svc",
			want:     "https://my-svc-123456.us-central1.run.app",
		},
		{
			name:     "GCLB audience returns empty",
			audience: "/projects/123/global/backendServices/456",
			want:     "",
		},
		{
			name:     "malformed returns empty",
			audience: "/projects/123/foo/bar",
			want:     "",
		},
		{
			name:     "empty returns empty",
			audience: "",
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := iapAudienceToCloudRunURL(tt.audience)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestValidateHostedHAPreflight(t *testing.T) {
	// Uses validHostedHAConfig (defined in server_ha_preflight_test.go)
	// and withHostedHAGuards to set the required package-level globals.

	t.Run("GCLB audience passes", func(t *testing.T) {
		withHostedHAGuards(t)
		cfg := validHostedHAConfig()
		cfg.Auth.Proxy.IAP.Audience = "/projects/123/global/backendServices/456"
		require.NoError(t, validateHostedHAPreflight(cfg))
	})

	t.Run("Cloud Run audience passes", func(t *testing.T) {
		withHostedHAGuards(t)
		cfg := validHostedHAConfig()
		require.NoError(t, validateHostedHAPreflight(cfg))
	})

	t.Run("malformed audience fails", func(t *testing.T) {
		withHostedHAGuards(t)
		cfg := validHostedHAConfig()
		cfg.Auth.Proxy.IAP.Audience = "/projects/123/foo/bar"
		err := validateHostedHAPreflight(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "supported IAP audience")
	})

	t.Run("empty audience fails", func(t *testing.T) {
		withHostedHAGuards(t)
		cfg := validHostedHAConfig()
		cfg.Auth.Proxy.IAP.Audience = ""
		err := validateHostedHAPreflight(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "server.auth.proxy.iap.audience")
	})

	t.Run("trailing slash is normalized in config", func(t *testing.T) {
		withHostedHAGuards(t)
		cfg := validHostedHAConfig()
		cfg.Auth.Proxy.IAP.Audience = "/projects/123/global/backendServices/456/"
		require.NoError(t, validateHostedHAPreflight(cfg))
		assert.Equal(t, "/projects/123/global/backendServices/456", cfg.Auth.Proxy.IAP.Audience,
			"preflight should strip trailing slash so downstream IAP validation uses the canonical audience")
	})

	t.Run("transport audience is normalized in config", func(t *testing.T) {
		withHostedHAGuards(t)
		cfg := validHostedHAConfig()
		cfg.Auth.Transport.OIDCAudience = "  123-abc.apps.googleusercontent.com/  "
		require.NoError(t, validateHostedHAPreflight(cfg))
		assert.Equal(t, "123-abc.apps.googleusercontent.com", cfg.Auth.Transport.OIDCAudience,
			"preflight should trim the transport audience so downstream token minting uses the canonical value")
	})
}
