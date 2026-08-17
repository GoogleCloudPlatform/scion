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
	cfg.Hub.HubID = "scion-hub"
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
			name: "missing hub_id",
			mutate: func(cfg *config.GlobalConfig) {
				cfg.Hub.HubID = ""
			},
			wantErr: "requires an explicit server.hub.hub_id",
		},
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
		// Note: "backend service audience" case was moved to
		// TestValidateHostedHAPreflightAcceptsGCLBAudience because GCLB
		// backend-service audiences are now accepted (GKE HA support).
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

func TestValidateHostedHAPreflightAcceptsGCLBAudience(t *testing.T) {
	withHostedHAGuards(t)
	cfg := validHostedHAConfig()
	cfg.Auth.Proxy.IAP.Audience = "/projects/486315127503/global/backendServices/987654321"
	cfg.Auth.Transport.OIDCAudience = cfg.Auth.Proxy.IAP.Audience

	var err error
	logged := captureLog(t, func() {
		err = validateHostedHAPreflight(cfg)
	})
	require.NoError(t, err)
	require.NotContains(t, logged, "looks like a bootstrap placeholder")
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

// validHostedHAOAuthConfig is the HA-safe counterpart of validHostedHAConfig
// for a hub that authenticates users itself: every universal check is
// satisfied and no IAP configuration is present.
func validHostedHAOAuthConfig() *config.GlobalConfig {
	cfg := config.DefaultGlobalConfig()
	cfg.Mode = "hosted"
	cfg.Hub.HubID = "scion-hub"
	cfg.Database.Driver = "postgres"
	cfg.Database.URL = "postgres://scion:secret@localhost/scionhub"
	cfg.Storage.Provider = "gcs"
	cfg.Storage.Bucket = "scion-prod-artifacts"
	cfg.Auth.Mode = "oauth"
	return &cfg
}

// TestValidateHostedHAPreflightAcceptsOAuthMode covers the core of #1087: IAP
// is not required for HA. A hub that authenticates users itself (OAuth/OIDC)
// starts in HA as long as the universal consistency checks are satisfied.
func TestValidateHostedHAPreflightAcceptsOAuthMode(t *testing.T) {
	withHostedHAGuards(t)

	require.NoError(t, validateHostedHAPreflight(validHostedHAOAuthConfig()), "oauth mode should pass HA preflight when universal checks are satisfied")
}

// TestValidateHostedHAPreflightStillRequiresIAPForProxy pins the other half of
// the split: proxy mode keeps every IAP requirement it has today.
func TestValidateHostedHAPreflightStillRequiresIAPForProxy(t *testing.T) {
	withHostedHAGuards(t)

	cfg := validHostedHAConfig()
	cfg.Auth.Proxy = nil
	cfg.Auth.Transport = nil

	err := validateHostedHAPreflight(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires server.auth.proxy.provider=iap")
}

// TestValidateHostedHAPreflightEnforcesUniversalForOAuth pins every universal
// check against oauth mode. Without a case per check, a universal check could
// be nested under the proxy guard by accident and the suite would stay green:
// the proxy-mode tests would still exercise it, and the oauth tests would only
// notice the one check they happen to break.
func TestValidateHostedHAPreflightEnforcesUniversalForOAuth(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(t *testing.T, cfg *config.GlobalConfig)
		wantErr string
	}{
		{
			name: "missing hub_id",
			mutate: func(t *testing.T, cfg *config.GlobalConfig) {
				cfg.Hub.HubID = ""
			},
			wantErr: "requires an explicit server.hub.hub_id",
		},
		{
			name: "sqlite",
			mutate: func(t *testing.T, cfg *config.GlobalConfig) {
				cfg.Database.Driver = "sqlite"
				// Postgres is what marks this deployment HA, so removing it
				// would also skip preflight entirely. K_SERVICE keeps
				// isHADeployment true and isolates the driver check.
				t.Setenv("K_SERVICE", "scion-hub")
			},
			wantErr: "requires server.database.driver=postgres",
		},
		{
			name: "empty postgres dsn",
			mutate: func(t *testing.T, cfg *config.GlobalConfig) {
				cfg.Database.URL = ""
			},
			wantErr: "requires server.database.url",
		},
		{
			name: "local storage",
			mutate: func(t *testing.T, cfg *config.GlobalConfig) {
				cfg.Storage.Provider = "local"
				cfg.Storage.Bucket = ""
			},
			wantErr: "requires server.storage.provider=gcs",
		},
		{
			name: "gcs without bucket",
			mutate: func(t *testing.T, cfg *config.GlobalConfig) {
				cfg.Storage.Bucket = ""
			},
			wantErr: "requires server.storage.provider=gcs",
		},
		{
			name: "missing session secret",
			mutate: func(t *testing.T, cfg *config.GlobalConfig) {
				webSessionSecret = ""
				t.Setenv("SCION_SERVER_SESSION_SECRET", "")
				t.Setenv("SESSION_SECRET", "")
			},
			wantErr: "durable session/signing secret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withHostedHAGuards(t)
			cfg := validHostedHAOAuthConfig()
			tt.mutate(t, cfg)

			err := validateHostedHAPreflight(cfg)
			require.Error(t, err, "universal checks should still enforce for oauth mode")
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
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

// TestIsHADeployment_RouteInventory is a tripwire on the source of truth for HA
// detection. It enumerates every condition that makes isHADeployment return
// true — one named subtest per route — so a route cannot be removed or swapped
// without failing CI. Addition of a new route is NOT caught by CI (no subtest
// exists for the unknown route, so nothing fails); the tripwire for addition is
// this comment and the test structure: a developer adding route #4 must add a
// matching subtest here and update the chart condition in lockstep.
//
// TRIPWIRE: the GKE Helm chart transcribes these same conditions into template
// logic gating the operator acknowledgement flag (acknowledgeHAUnlanded). If
// you add a route here and not to the chart, the chart's condition
// under-triggers: it renders an HA config without the acknowledgement, and that
// config cannot boot. Remove or swap a route without touching the chart and it
// over-triggers, demanding an acknowledgement for a deployment that is not HA.
// So: change isHADeployment -> update the chart condition in lockstep -> add or
// update the matching subtest below. To locate the chart condition (once the
// GKE chart has landed), grep the deploy/helm tree for acknowledgeHAUnlanded.
//
// Routes as of this writing:
//  1. K_SERVICE env var set (Cloud Run)
//  2. database.driver == "postgres"
//  3. storage.provider == "gcs" && auth.mode == "proxy"
//
// This asserts the route SET, not the route count: each subtest is named for
// the identity of its route, so a removal or a swap fails the replaced route's
// subtest. An addition fails nothing — the new route simply has no subtest —
// which is why adding one is a documented obligation rather than an enforced
// one.
func TestIsHADeployment_RouteInventory(t *testing.T) {
	// NOTE: this test intentionally overlaps with TestIsHADeployment above.
	// TestIsHADeployment validates functional correctness of HA detection
	// across combinations. This test is a route-inventory tripwire whose
	// primary value is the TRIPWIRE comment block above — it documents the
	// chart condition that must be updated in lockstep. Do not merge them.

	// baseConfig returns a config with none of the HA routes active — the
	// package defaults are sqlite + local storage + unset auth mode. Each
	// subtest then turns on exactly one route, so a failure names that route.
	baseConfig := func(t *testing.T) *config.GlobalConfig {
		t.Helper()
		// K_SERVICE is not set under test by default; clear it anyway so a
		// leaked value cannot make a subtest pass for the wrong reason.
		t.Setenv("K_SERVICE", "")
		cfg := config.DefaultGlobalConfig()
		return &cfg
	}

	t.Run("K_SERVICE", func(t *testing.T) {
		cfg := baseConfig(t)
		t.Setenv("K_SERVICE", "my-svc")

		require.True(t, isHADeployment(cfg), "K_SERVICE set (Cloud Run) must be an HA route")
	})

	t.Run("postgres_driver", func(t *testing.T) {
		cfg := baseConfig(t)
		cfg.Database.Driver = "postgres"

		require.True(t, isHADeployment(cfg), "database.driver=postgres must be an HA route")
	})

	t.Run("gcs_plus_proxy", func(t *testing.T) {
		cfg := baseConfig(t)
		cfg.Storage.Provider = "gcs"
		cfg.Auth.Mode = "proxy"

		require.True(t, isHADeployment(cfg), "storage.provider=gcs with auth.mode=proxy must be an HA route")
	})

	t.Run("no_route_active", func(t *testing.T) {
		cfg := baseConfig(t)

		require.False(t, isHADeployment(cfg), "no HA route active must not be detected as HA")
	})
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

func TestIAPAudienceShape(t *testing.T) {
	require.True(t, isSupportedIAPAudience("/projects/123/locations/us-central1/services/scion"))
	require.True(t, isSupportedIAPAudience("/projects/123/global/backendServices/456"))
	require.False(t, isSupportedIAPAudience(strings.TrimSpace("")))
}

func TestIsLikelyPlaceholderAudience(t *testing.T) {
	tests := []struct {
		name     string
		audience string
		want     bool
	}{
		{"canonical placeholder", "/projects/000000000/global/backendServices/0", true},
		{"zero backend ID", "/projects/486315127503/global/backendServices/0", true},
		{"padded zero backend service ID", "/projects/486315127503/global/backendServices/000000", true},
		{"all-zeros project", "/projects/000000000/global/backendServices/12345", true},
		{"all-zeros project short", "/projects/0/global/backendServices/12345", true},
		{"dummy project number", "/projects/123456789/global/backendServices/12345", true},
		{"real audience", "/projects/486315127503/global/backendServices/987654321", false},
		{"cloud run (not checked)", "/projects/123/locations/us-central1/services/scion-hub", false},
		{"cloud run with dummy project (not checked)", "/projects/123456789/locations/us-central1/services/scion-hub", false},
		{"empty", "", false},
		{"malformed", "/projects//global/backendServices/12345", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isLikelyPlaceholderAudience(tt.audience))
		})
	}
}

// A placeholder audience is intentionally non-fatal: it is the supported GKE
// bootstrap path, where the real backend-service ID does not exist until the
// ingress has reconciled.
func TestValidateHostedHAPreflightAcceptsPlaceholderGCLBAudience(t *testing.T) {
	withHostedHAGuards(t)
	cfg := validHostedHAConfig()
	cfg.Auth.Proxy.IAP.Audience = "/projects/000000000/global/backendServices/0"
	cfg.Auth.Transport.OIDCAudience = cfg.Auth.Proxy.IAP.Audience

	// Non-fatality alone is not the contract: the operator must also be told
	// the deployment will 401 until the real backend-service ID is wired in.
	var err error
	logged := captureLog(t, func() {
		err = validateHostedHAPreflight(cfg)
	})
	require.NoError(t, err)
	require.Contains(t, logged, "looks like a bootstrap placeholder")
	require.Contains(t, logged, cfg.Auth.Proxy.IAP.Audience)
}
