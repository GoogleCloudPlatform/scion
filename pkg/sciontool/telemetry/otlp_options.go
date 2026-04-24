/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

var errOTLPCACertsNotFound = errors.New("parsing OTLP CA file: no certificates found")
var errOTLPMissingClientKeyPair = errors.New("OTLP client certificate and key must be provided together")

func LoadOTLPTLSConfig(caFile, certFile, keyFile string) (*tls.Config, error) {
	tlsConfig := &tls.Config{}

	if caFile != "" {
		pemBytes, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("reading OTLP CA file: %w", err)
		}

		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pemBytes) {
			return nil, errOTLPCACertsNotFound
		}
		tlsConfig.RootCAs = roots
	}

	if certFile == "" && keyFile == "" {
		return tlsConfig, nil
	}
	if certFile == "" || keyFile == "" {
		return nil, errOTLPMissingClientKeyPair
	}

	clientCert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading OTLP client certificate: %w", err)
	}
	tlsConfig.Certificates = []tls.Certificate{clientCert}
	return tlsConfig, nil
}

func loadSecureGCPDialOptions(ctx context.Context, config *Config) ([]grpc.DialOption, error) {
	if config.GCPCredentialsFile == "" || config.Insecure {
		return nil, nil
	}

	dialOpts, err := loadGCPDialOptions(ctx, config.GCPCredentialsFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load GCP credentials: %w", err)
	}

	return dialOpts, nil
}

func loadSecureOTLPTLSConfig(config *Config) (*tls.Config, error) {
	tlsConfig, err := LoadOTLPTLSConfig(config.CAFile, config.CertFile, config.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load OTLP TLS config: %w", err)
	}
	return tlsConfig, nil
}

func otlpGRPCTransportCredentials(config *Config) (grpc.DialOption, error) {
	if config.Insecure {
		return grpc.WithTransportCredentials(insecure.NewCredentials()), nil
	}

	tlsConfig, err := loadSecureOTLPTLSConfig(config)
	if err != nil {
		return nil, err
	}
	return grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), nil
}

func appendOTLPTraceGRPCSecurityOption(opts []otlptracegrpc.Option, config *Config) ([]otlptracegrpc.Option, error) {
	if config.Insecure {
		return append(opts, otlptracegrpc.WithInsecure()), nil
	}

	tlsConfig, err := loadSecureOTLPTLSConfig(config)
	if err != nil {
		return nil, err
	}
	return append(opts, otlptracegrpc.WithTLSCredentials(credentials.NewTLS(tlsConfig))), nil
}

func appendOTLPLogGRPCSecurityOption(opts []otlploggrpc.Option, config *Config) ([]otlploggrpc.Option, error) {
	if config.Insecure {
		return append(opts, otlploggrpc.WithInsecure()), nil
	}

	tlsConfig, err := loadSecureOTLPTLSConfig(config)
	if err != nil {
		return nil, err
	}
	return append(opts, otlploggrpc.WithTLSCredentials(credentials.NewTLS(tlsConfig))), nil
}

func appendOTLPMetricGRPCSecurityOption(opts []otlpmetricgrpc.Option, config *Config) ([]otlpmetricgrpc.Option, error) {
	if config.Insecure {
		return append(opts, otlpmetricgrpc.WithInsecure()), nil
	}

	tlsConfig, err := loadSecureOTLPTLSConfig(config)
	if err != nil {
		return nil, err
	}
	return append(opts, otlpmetricgrpc.WithTLSCredentials(credentials.NewTLS(tlsConfig))), nil
}

func appendOTLPTraceHTTPSecurityOption(opts []otlptracehttp.Option, config *Config) ([]otlptracehttp.Option, error) {
	if config.Insecure {
		return append(opts, otlptracehttp.WithInsecure()), nil
	}

	tlsConfig, err := loadSecureOTLPTLSConfig(config)
	if err != nil {
		return nil, err
	}
	return append(opts, otlptracehttp.WithTLSClientConfig(tlsConfig)), nil
}
