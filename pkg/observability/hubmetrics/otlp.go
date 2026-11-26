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

package hubmetrics

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/util/otlptls"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/sdk/metric"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Environment variables for the Hub's operator-configured OTLP metrics sink.
// They intentionally match pkg/util/logging/otel_provider.go and
// pkg/sciontool/telemetry/config.go so operators configure one consistent set
// of SCION_OTEL_* variables across Scion binaries.
const (
	EnvOTelEndpoint      = "SCION_OTEL_ENDPOINT"
	EnvOTelInsecure      = "SCION_OTEL_INSECURE"
	EnvOTelSkipTLSVerify = "SCION_OTEL_SKIP_TLS_VERIFY"
	EnvOTelCAFile        = "SCION_OTEL_CA_FILE"
	EnvOTelCertFile      = "SCION_OTEL_CERT_FILE"
	EnvOTelKeyFile       = "SCION_OTEL_KEY_FILE"
	EnvOTelHeaders       = "SCION_OTEL_HEADERS"
)

// OTLPConfig configures an operator-managed OTLP gRPC metrics sink on the Hub
// MeterProvider (used alongside or instead of GCP Cloud Monitoring).
type OTLPConfig struct {
	Endpoint      string
	Insecure      bool
	SkipTLSVerify bool
	CAFile        string
	CertFile      string
	KeyFile       string
	Headers       map[string]string
}

// Enabled reports whether an OTLP endpoint is configured.
func (c OTLPConfig) Enabled() bool {
	return strings.TrimSpace(c.Endpoint) != ""
}

// LoadOTLPConfigFromEnv reads Hub OTLP metrics sink configuration from the
// SCION_OTEL_* environment variables. Returns an error only when
// SCION_OTEL_ENDPOINT is set and SCION_OTEL_HEADERS is malformed.
func LoadOTLPConfigFromEnv() (OTLPConfig, error) {
	return loadOTLPConfig(os.Getenv)
}

func loadOTLPConfig(getenv func(string) string) (OTLPConfig, error) {
	cfg := OTLPConfig{
		Endpoint: strings.TrimSpace(getenv(EnvOTelEndpoint)),
		CAFile:   strings.TrimSpace(getenv(EnvOTelCAFile)),
		CertFile: strings.TrimSpace(getenv(EnvOTelCertFile)),
		KeyFile:  strings.TrimSpace(getenv(EnvOTelKeyFile)),
	}
	if cfg.Endpoint == "" {
		return cfg, nil
	}
	if v, ok := parseBoolValue(getenv(EnvOTelInsecure)); ok {
		cfg.Insecure = v
	}
	if v, ok := parseBoolValue(getenv(EnvOTelSkipTLSVerify)); ok {
		cfg.SkipTLSVerify = v
	}
	if rawHeaders := strings.TrimSpace(getenv(EnvOTelHeaders)); rawHeaders != "" {
		headers, err := otlptls.ParseHeaders(rawHeaders)
		if err != nil {
			return OTLPConfig{}, fmt.Errorf("invalid %s: %w", EnvOTelHeaders, err)
		}
		cfg.Headers = headers
	}
	return cfg, nil
}

// WithOTLPConfig attaches an operator-configured OTLP gRPC metrics exporter
// reader to the Hub MeterProvider. When combined with a non-empty GCP project
// ID, the MeterProvider exports to both Cloud Monitoring and OTLP using the
// same OTel Resource (including service.instance.id). When gcpProjectID is
// empty and cfg.Enabled() is true, the MeterProvider exports to OTLP alone.
func WithOTLPConfig(cfg OTLPConfig) Option {
	return func(o *options) {
		if cfg.Enabled() {
			cp := cfg
			if len(cfg.Headers) > 0 {
				cp.Headers = make(map[string]string, len(cfg.Headers))
				for k, v := range cfg.Headers {
					cp.Headers[k] = v
				}
			}
			o.otlp = &cp
		}
	}
}

func newOTLPExporter(ctx context.Context, cfg OTLPConfig) (metric.Exporter, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("OTLP endpoint is required")
	}

	opts := []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpoint(endpoint),
	}
	if cfg.Insecure {
		if cfg.SkipTLSVerify || cfg.CAFile != "" || cfg.CertFile != "" || cfg.KeyFile != "" {
			return nil, fmt.Errorf("plaintext OTLP transport conflicts with TLS verification or client certificate options")
		}
		opts = append(opts, otlpmetricgrpc.WithTLSCredentials(insecure.NewCredentials()))
	} else if cfg.SkipTLSVerify || cfg.CAFile != "" || cfg.CertFile != "" || cfg.KeyFile != "" {
		tlsCfg, err := otlptls.LoadClientConfig(cfg.CAFile, cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading OTLP TLS config: %w", err)
		}
		tlsCfg.InsecureSkipVerify = cfg.SkipTLSVerify //nolint:gosec // explicit operator configuration
		opts = append(opts, otlpmetricgrpc.WithTLSCredentials(credentials.NewTLS(tlsCfg)))
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlpmetricgrpc.WithHeaders(cfg.Headers))
	}

	exp, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating OTLP metric exporter: %w", err)
	}
	return &loggingExporter{delegate: exp}, nil
}

// warnOTLPConfig logs a structured warning when OTLP exporter setup fails while
// another exporter (e.g. Cloud Monitoring) remains active.
func warnOTLPConfig(err error) {
	slog.Warn("hub metrics: OTLP exporter disabled", "error", err)
}

func parseBoolValue(raw string) (bool, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return false, false
	}
	switch s {
	case "yes", "y", "on":
		return true, true
	case "no", "n", "off":
		return false, true
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return false, false
	}
	return v, true
}
