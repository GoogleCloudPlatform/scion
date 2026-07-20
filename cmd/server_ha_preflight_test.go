package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/stretchr/testify/require"
)

func validHostedHAConfig() *config.GlobalConfig {
	cfg := config.DefaultGlobalConfig()
	cfg.Mode = "hosted"
	cfg.Database.Driver = "postgres"
	cfg.Database.URL = "postgres://scion:secret@localhost/scionhub"
	cfg.Storage.Provider = "gcs"
	cfg.Storage.Bucket = "scion-prod-artifacts"
	cfg.Auth.Mode = "proxy"
	cfg.Auth.Proxy = &config.ProxyAuthConfig{
		Provider: "iap",
		IAP: &config.IAPAuthConfig{
			Audience: "/projects/123456789/locations/us-central1/services/scion-hub",
		},
	}
	cfg.Auth.Transport = &config.TransportAuthConfig{
		Mode:           "iap",
		OIDCAudience:   "/projects/123456789/locations/us-central1/services/scion-hub",
		PlatformAuthSA: "scion-transport@example.iam.gserviceaccount.com",
	}
	return &cfg
}

func withHostedHAGuards(t *testing.T) {
	t.Helper()
	resetServerFlags()
	hostedMode = true
	enableHub = true
	t.Setenv("SCION_SERVER_SESSION_SECRET", "durable-test-secret")
	t.Cleanup(resetServerFlags)
}

func TestValidateHostedHAPreflightAcceptsCloudRunHAConfig(t *testing.T) {
	withHostedHAGuards(t)

	require.NoError(t, validateHostedHAPreflight(validHostedHAConfig()))
}

func TestValidateHostedHAPreflightRejectsUnsafeBackends(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.GlobalConfig)
		wantErr string
	}{
		{
			name: "sqlite",
			mutate: func(cfg *config.GlobalConfig) {
				cfg.Database.Driver = "sqlite"
			},
			wantErr: "requires server.database.driver=postgres",
		},
		{
			name: "empty postgres dsn",
			mutate: func(cfg *config.GlobalConfig) {
				cfg.Database.URL = ""
			},
			wantErr: "requires server.database.url",
		},
		{
			name: "local storage",
			mutate: func(cfg *config.GlobalConfig) {
				cfg.Storage.Provider = "local"
				cfg.Storage.Bucket = ""
			},
			wantErr: "requires server.storage.provider=gcs",
		},
		{
			name: "missing transport",
			mutate: func(cfg *config.GlobalConfig) {
				cfg.Auth.Transport = nil
			},
			wantErr: "requires server.auth.transport",
		},
		{
			name: "wrong transport mode",
			mutate: func(cfg *config.GlobalConfig) {
				cfg.Auth.Transport.Mode = "cloudrun_invoker"
			},
			wantErr: "requires server.auth.transport.mode=iap",
		},
		{
			name: "backend service audience",
			mutate: func(cfg *config.GlobalConfig) {
				cfg.Auth.Proxy.IAP.Audience = "/projects/123456789/global/backendServices/987654321"
				cfg.Auth.Transport.OIDCAudience = cfg.Auth.Proxy.IAP.Audience
			},
			wantErr: "Cloud Run native IAP audience",
		},
		// Note: "transport audience mismatch" case was removed because PR #814
		// intentionally decoupled transport.oidc_audience from proxy.iap.audience.
		// transport.oidc_audience is the IAP OAuth client ID used for minting OIDC
		// tokens, while proxy.iap.audience is the Cloud Run resource path for
		// validating incoming IAP-signed JWTs. These are expected to differ.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withHostedHAGuards(t)
			cfg := validHostedHAConfig()
			tt.mutate(cfg)

			err := validateHostedHAPreflight(cfg)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidateHostedHAPreflightRequiresSessionSecret(t *testing.T) {
	resetServerFlags()
	hostedMode = true
	enableHub = true
	webSessionSecret = ""
	t.Setenv("SCION_SERVER_SESSION_SECRET", "")
	t.Setenv("SESSION_SECRET", "")
	t.Cleanup(resetServerFlags)

	err := validateHostedHAPreflight(validHostedHAConfig())
	require.Error(t, err)
	require.Contains(t, err.Error(), "durable session/signing secret")
}

func TestValidateHostedHAPreflightSkippedOutsideHostedHub(t *testing.T) {
	resetServerFlags()
	t.Cleanup(resetServerFlags)

	cfg := validHostedHAConfig()
	cfg.Database.Driver = "sqlite"
	cfg.Storage.Provider = "local"
	cfg.Storage.Bucket = ""

	require.NoError(t, validateHostedHAPreflight(cfg))
}

func TestValidateHostedHAPreflightSkippedForNonHA(t *testing.T) {
	resetServerFlags()
	hostedMode = true
	enableHub = true
	t.Setenv("SCION_SERVER_SESSION_SECRET", "test-secret")
	t.Setenv("K_SERVICE", "")
	t.Cleanup(resetServerFlags)

	cfg := config.DefaultGlobalConfig()
	cfg.Mode = "hosted"
	cfg.Database.Driver = "sqlite"
	cfg.Database.URL = ""
	cfg.Storage.Provider = "local"
	cfg.Auth.Mode = "oauth"

	require.NoError(t, validateHostedHAPreflight(&cfg))
}

func TestIsHADeployment(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(t *testing.T, cfg *config.GlobalConfig)
		expected bool
	}{
		{
			name: "sqlite + local + oauth is not HA",
			setup: func(t *testing.T, cfg *config.GlobalConfig) {
				cfg.Database.Driver = "sqlite"
				cfg.Storage.Provider = "local"
				cfg.Auth.Mode = "oauth"
			},
			expected: false,
		},
		{
			name: "postgres triggers HA",
			setup: func(t *testing.T, cfg *config.GlobalConfig) {
				cfg.Database.Driver = "postgres"
				cfg.Storage.Provider = "local"
				cfg.Auth.Mode = "oauth"
			},
			expected: true,
		},
		{
			name: "K_SERVICE triggers HA",
			setup: func(t *testing.T, cfg *config.GlobalConfig) {
				cfg.Database.Driver = "sqlite"
				cfg.Storage.Provider = "local"
				cfg.Auth.Mode = "oauth"
				t.Setenv("K_SERVICE", "scion-hub")
			},
			expected: true,
		},
		{
			name: "gcs + proxy triggers HA",
			setup: func(t *testing.T, cfg *config.GlobalConfig) {
				cfg.Database.Driver = "sqlite"
				cfg.Storage.Provider = "gcs"
				cfg.Auth.Mode = "proxy"
			},
			expected: true,
		},
		{
			name: "gcs without proxy is not HA",
			setup: func(t *testing.T, cfg *config.GlobalConfig) {
				cfg.Database.Driver = "sqlite"
				cfg.Storage.Provider = "gcs"
				cfg.Auth.Mode = "oauth"
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("K_SERVICE", "")
			cfg := config.DefaultGlobalConfig()
			tt.setup(t, &cfg)
			require.Equal(t, tt.expected, isHADeployment(&cfg))
		})
	}
}

func TestNewEventPublisherFailsClosedForHostedHA(t *testing.T) {
	withHostedHAGuards(t)
	cfg := validHostedHAConfig()
	cfg.Database.URL = "not a postgres dsn"

	_, err := newEventPublisher(context.Background(), cfg, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "required Postgres event publisher")
}

func TestNewEventPublisherFallsBackOutsideHostedHA(t *testing.T) {
	resetServerFlags()
	t.Cleanup(resetServerFlags)

	cfg := validHostedHAConfig()
	cfg.Database.URL = "not a postgres dsn"

	pub, err := newEventPublisher(context.Background(), cfg, nil)
	require.NoError(t, err)
	require.IsType(t, &hub.ChannelEventPublisher{}, pub)
}

func TestCloudRunIAPAudienceShape(t *testing.T) {
	require.True(t, isCloudRunIAPAudience("/projects/123/locations/us-central1/services/scion"))
	require.False(t, isCloudRunIAPAudience("/projects/123/global/backendServices/456"))
	require.False(t, isCloudRunIAPAudience(strings.TrimSpace("")))
}
