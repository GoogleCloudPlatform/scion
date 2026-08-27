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
		// Cloud Run Instance audience — the path says "services" even though
		// the backend is a Cloud Run Instance, not a Service. This is correct:
		// IAP uses a fixed resource vocabulary ("/services/") for every backend
		// type, including Instances. It is NOT a bug and NOT a mismatch to
		// correct.
		//
		// Changing "services" to "instances" in the audience would produce an
		// audience mismatch on every request: the IAP edge would still stamp
		// the JWT with a "/services/" audience, but the hub would be
		// configured to expect "/instances/", and every comparison would fail
		// with a 401 that does not obviously point back to this code.
		//
		// Region-scope note: IAP policy binding is at region level
		// (projects/{PROJECT}/iap_web/cloud_run-{REGION}), not per-instance —
		// per-instance setIamPolicy returns 404 for Cloud Run Instances. This
		// is acceptable when the project hosts a single tenant, because a
		// region-level grant admits the holder to exactly one resource.
		//
		// ⚠️ Revisit trigger: if this tier ever hosts more than one tenant in
		// one project, region scope is immediately wrong and per-resource auth
		// must come back (see design doc §11.2, §11.1).
		//
		// Reference: design doc §11.3 (identity flow), OQ-17 (audience
		// confirmation), §11.2 (region-scope IAP policy).
		{
			name:     "cloud run Instance audience uses services path",
			audience: "/projects/123456789/locations/us-east4/services/my-instance",
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

func TestInitWebServer_DevAuth_NonLoopback_Rejected(t *testing.T) {
	tests := []struct {
		name        string
		host        string
		devAuth     string
		wantErr     bool
		errContains string
	}{
		{
			name:        "dev auth with 0.0.0.0 rejected",
			host:        "0.0.0.0",
			devAuth:     "some-token",
			wantErr:     true,
			errContains: "dev auth cannot be enabled",
		},
		{
			name:        "dev auth with empty host (defaults to 0.0.0.0) rejected",
			host:        "",
			devAuth:     "some-token",
			wantErr:     true,
			errContains: "non-loopback address",
		},
		{
			name:        "dev auth with public IP rejected",
			host:        "192.168.1.1",
			devAuth:     "some-token",
			wantErr:     true,
			errContains: "non-loopback address",
		},
		{
			name:    "dev auth with 127.0.0.1 allowed",
			host:    "127.0.0.1",
			devAuth: "some-token",
			wantErr: false,
		},
		{
			name:    "no dev auth with 0.0.0.0 allowed",
			host:    "0.0.0.0",
			devAuth: "",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.GlobalConfig{}
			cfg.Hub.Host = tt.host

			_, err := initWebServer(context.Background(), cfg, nil, tt.devAuth, false, "", nil, nil)

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
			} else {
				// initWebServer may fail for other reasons (e.g., missing deps)
				// but it should NOT fail with a dev-auth error.
				if err != nil {
					assert.NotContains(t, err.Error(), "dev auth cannot be enabled",
						"should not reject dev auth on loopback host")
				}
			}
		})
	}
}
