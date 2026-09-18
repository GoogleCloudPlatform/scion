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
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// ServerAuthConfig holds configuration for server-side gRPC authentication
// and TLS. Used to create grpc.ServerOption slices for the bridge's standalone
// gRPC server.
type ServerAuthConfig struct {
	// Validator validates bearer tokens from incoming RPCs. When nil, no
	// token validation is performed (only suitable for localhost/h2c dev).
	Validator TokenValidator

	// TLSCertFile is the path to the server's TLS certificate (PEM).
	// Required for native TLS (Kubernetes). Ignored on Cloud Run where
	// TLS is terminated by the platform.
	TLSCertFile string

	// TLSKeyFile is the path to the server's TLS private key (PEM).
	TLSKeyFile string

	// TLSClientCAFile is the path to the CA certificate for verifying
	// client certificates (mTLS). When set, the server requires and
	// verifies client certificates against this CA.
	TLSClientCAFile string
}

// ServerOptions returns grpc.ServerOption slices for auth interceptors and TLS
// credentials based on the config. When Validator is set, all methods are
// protected by bearer token validation. When TLS fields are set, native
// TLS/mTLS is configured.
func ServerOptions(cfg ServerAuthConfig) ([]grpc.ServerOption, error) {
	var opts []grpc.ServerOption

	// Auth interceptors.
	if cfg.Validator != nil {
		opts = append(opts,
			grpc.UnaryInterceptor(UnaryAuthInterceptor(cfg.Validator)),
			grpc.StreamInterceptor(StreamAuthInterceptor(cfg.Validator)),
		)
	}

	// TLS credentials.
	if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
		tlsCfg, err := serverTLSConfig(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.TLSClientCAFile)
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}

	return opts, nil
}

// UnaryAuthInterceptor returns a gRPC unary server interceptor that validates
// bearer tokens on every unary RPC call using the provided validator.
func UnaryAuthInterceptor(validator TokenValidator) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		token, err := extractBearerToken(ctx)
		if err != nil {
			return nil, err
		}
		if err := validator.ValidateToken(ctx, token); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamAuthInterceptor returns a gRPC stream server interceptor that
// validates bearer tokens on stream establishment.
func StreamAuthInterceptor(validator TokenValidator) grpc.StreamServerInterceptor {
	return func(
		srv interface{},
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		token, err := extractBearerToken(ss.Context())
		if err != nil {
			return err
		}
		if err := validator.ValidateToken(ss.Context(), token); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// serverTLSConfig creates a tls.Config for the gRPC server. When clientCAFile
// is non-empty, mTLS is enabled (RequireAndVerifyClientCert).
func serverTLSConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load server cert/key: %w", err)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	if clientCAFile != "" {
		caCert, err := os.ReadFile(clientCAFile)
		if err != nil {
			return nil, fmt.Errorf("read client CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse client CA certificate")
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return tlsCfg, nil
}
