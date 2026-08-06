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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
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

// validHAConfig returns a fully-valid HA config that passes all preflight
// checks. Tests modify individual fields to exercise specific validation paths.
func validHAConfig(audience string) *config.GlobalConfig {
	return &config.GlobalConfig{
		Database: config.DatabaseConfig{
			Driver: "postgres",
			URL:    "postgres://localhost/scion",
		},
		Storage: config.StorageConfig{
			Provider: "gcs",
			Bucket:   "test-bucket",
		},
		Auth: config.DevAuthConfig{
			Mode: "proxy",
			Proxy: &config.ProxyAuthConfig{
				Provider: "iap",
				IAP: &config.IAPAuthConfig{
					Audience: audience,
				},
			},
			Transport: &config.TransportAuthConfig{
				Mode:           "iap",
				OIDCAudience:   "client-id.apps.googleusercontent.com",
				PlatformAuthSA: "sa@project.iam.gserviceaccount.com",
			},
		},
	}
}

func TestValidateHostedHAPreflight(t *testing.T) {
	// Save and restore globals that validateHostedHAPreflight depends on.
	origHostedMode := hostedMode
	origEnableHub := enableHub
	origSessionSecret := webSessionSecret
	t.Cleanup(func() {
		hostedMode = origHostedMode
		enableHub = origEnableHub
		webSessionSecret = origSessionSecret
	})

	// Set globals so hostedHAGuardsRequired returns true.
	hostedMode = true
	enableHub = true
	webSessionSecret = "test-secret"

	t.Run("GCLB audience passes", func(t *testing.T) {
		cfg := validHAConfig("/projects/123/global/backendServices/456")
		err := validateHostedHAPreflight(cfg)
		require.NoError(t, err)
	})

	t.Run("Cloud Run audience passes", func(t *testing.T) {
		cfg := validHAConfig("/projects/123/locations/us-central1/services/my-svc")
		err := validateHostedHAPreflight(cfg)
		require.NoError(t, err)
	})

	t.Run("malformed audience fails", func(t *testing.T) {
		cfg := validHAConfig("/projects/123/foo/bar")
		err := validateHostedHAPreflight(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "supported IAP audience")
	})

	t.Run("empty audience fails", func(t *testing.T) {
		cfg := validHAConfig("")
		err := validateHostedHAPreflight(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "server.auth.proxy.iap.audience")
	})
}
