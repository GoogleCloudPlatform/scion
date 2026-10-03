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

package grpcbroker

import (
	"fmt"
	"log/slog"
	"os"

	"cloud.google.com/go/compute/metadata"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// AuthTypeGoogleIDToken is the auth type for Google OIDC ID tokens
// (Cloud Run invoker tokens or Kubernetes service-account tokens).
const AuthTypeGoogleIDToken = "google_id_token"

// isOnGCE is a package-level function for detecting GCE. Tests override this.
var isOnGCE = func() bool { return metadata.OnGCE() }

// adcSourceNew is the optional ADC source constructor. Injected at init time
// to avoid linking google.golang.org/api/idtoken when not needed.
// Tests override this.
var adcSourceNew transportauth.ADCSourceConstructor

// SetADCSourceConstructor sets the ADC source constructor for the factory.
// Called from cmd/server_foreground.go to inject the adcsource package
// without creating an import cycle in this package.
func SetADCSourceConstructor(fn transportauth.ADCSourceConstructor) {
	adcSourceNew = fn
}

// NewAdapterFromEntry creates a GRPCBrokerClient from a PluginEntry.
// This is the factory function injected into plugin.Manager.NewGRPCBrokerAdapter
// to avoid import cycles.
func NewAdapterFromEntry(entry plugin.PluginEntry, logger *slog.Logger) (plugin.GRPCBrokerClient, error) {
	var tlsCfg *TLSConfig
	if entry.TLSCertFile != "" || entry.TLSCAFile != "" || entry.TLSSkipVerify {
		tlsCfg = &TLSConfig{
			CertFile:   entry.TLSCertFile,
			KeyFile:    entry.TLSKeyFile,
			CAFile:     entry.TLSCAFile,
			SkipVerify: entry.TLSSkipVerify,
		}
	}

	// Resolve per-RPC authenticator from config.
	auth, err := resolveAuthenticator(entry, logger)
	if err != nil {
		return nil, fmt.Errorf("grpc broker auth: %w", err)
	}

	adapter := NewGRPCBrokerAdapter(AdapterConfig{
		Address:       entry.Address,
		TLS:           tlsCfg,
		Logger:        logger,
		Authenticator: auth,
	})

	return adapter, nil
}

// resolveAuthenticator creates a PerCallAuthenticator based on PluginEntry
// auth fields. Fails closed for remote addresses without explicit auth.
func resolveAuthenticator(entry plugin.PluginEntry, logger *slog.Logger) (PerCallAuthenticator, error) {
	switch entry.AuthType {
	case "", "none":
		if isLocalAddress(entry.Address) {
			// Local addresses are allowed without auth (h2c dev mode).
			return nil, nil
		}
		if entry.AuthType == "none" {
			// Explicit "none" for remote is rejected — fail closed.
			return nil, fmt.Errorf("auth_type %q is not allowed for remote address %q; "+
				"use %q or configure TLS with mTLS client identity",
				"none", entry.Address, AuthTypeGoogleIDToken)
		}
		// Missing auth_type for remote address — fail closed.
		return nil, fmt.Errorf("auth_type is required for remote gRPC address %q; "+
			"set auth_type to %q with auth_audience, or use a local address for development",
			entry.Address, AuthTypeGoogleIDToken)

	case AuthTypeGoogleIDToken:
		if entry.AuthAudience == "" {
			return nil, fmt.Errorf("auth_type %q requires auth_audience to be set", AuthTypeGoogleIDToken)
		}
		src, err := resolveGoogleIDTokenSource(entry.AuthAudience, logger)
		if err != nil {
			return nil, err
		}
		// Require TLS for non-local addresses.
		requireTLS := !isLocalAddress(entry.Address)
		// Send dual headers for Cloud Run: X-Serverless-Authorization for
		// platform invoker auth, Authorization for application-level auth.
		// Safe for non-Cloud Run targets (extra header is ignored).
		return NewTokenSourceCredentials(src, requireTLS, WithCloudRunHeader()), nil

	default:
		return nil, fmt.Errorf("unsupported auth_type %q; supported: %q, %q",
			entry.AuthType, "none", AuthTypeGoogleIDToken)
	}
}

// resolveGoogleIDTokenSource creates a transportauth.TokenSource for Google
// OIDC ID tokens. On GCE it uses the metadata server for automatic credential
// refresh; off-GCE it falls back to ADC. The source handles token caching and
// refresh internally — credentials survive rotation without restart.
func resolveGoogleIDTokenSource(audience string, logger *slog.Logger) (transportauth.TokenSource, error) {
	// On GCE: use metadata server (supports Workload Identity, SA).
	// Check that scion metadata hijacking is not active.
	if isOnGCE() {
		if metaMode := os.Getenv(transportauth.EnvMetadataMode); metaMode == "" {
			logger.Info("using GCE metadata server for gRPC broker ID tokens",
				"audience", audience)
			return transportauth.NewMetadataSource(audience), nil
		}
	}

	// Off-GCE: fall back to ADC if available.
	if adcSourceNew != nil {
		logger.Info("using ADC for gRPC broker ID tokens",
			"audience", audience)
		src, err := adcSourceNew(audience)
		if err != nil {
			return nil, fmt.Errorf("ADC source for audience %q: %w", audience, err)
		}
		return src, nil
	}

	return nil, fmt.Errorf("auth_type %q configured but no credential source available: "+
		"not on GCE and no ADC constructor registered", AuthTypeGoogleIDToken)
}
