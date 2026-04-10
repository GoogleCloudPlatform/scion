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

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/trace"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"golang.org/x/oauth2/google"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/oauth"
)

var errOTLPCACertsNotFound = errors.New("parsing OTLP CA file: no certificates found")

func loadOTLPTLSConfig(caFile string) (*tls.Config, error) {
	tlsConfig := &tls.Config{}
	if caFile == "" {
		return tlsConfig, nil
	}

	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading OTLP CA file: %w", err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pemBytes) {
		return nil, errOTLPCACertsNotFound
	}
	tlsConfig.RootCAs = roots
	return tlsConfig, nil
}

// loadGCPDialOptions loads GCP credentials from a service account key file
// and returns gRPC dial options for per-RPC authentication. Returns (nil, nil)
// if credFile is empty. The credentials are scoped for Cloud Trace, Logging,
// and Monitoring write access.
func loadGCPDialOptions(ctx context.Context, credFile string) ([]grpc.DialOption, error) {
	if credFile == "" {
		return nil, nil
	}
	keyBytes, err := os.ReadFile(credFile)
	if err != nil {
		return nil, fmt.Errorf("reading GCP credentials file: %w", err)
	}
	creds, err := google.CredentialsFromJSON(ctx, keyBytes,
		"https://www.googleapis.com/auth/trace.append",
		"https://www.googleapis.com/auth/logging.write",
		"https://www.googleapis.com/auth/monitoring.write",
	)
	if err != nil {
		return nil, fmt.Errorf("parsing GCP credentials: %w", err)
	}
	perRPC := oauth.TokenSource{TokenSource: creds.TokenSource}
	return []grpc.DialOption{grpc.WithPerRPCCredentials(perRPC)}, nil
}

// CloudExporter exports traces, metrics, and logs to a cloud backend.
// It supports two modes:
//   - GCP-native: uses Cloud Trace and Cloud Logging APIs directly
//   - Generic OTLP: forwards raw proto data via standard OTLP gRPC/HTTP
type CloudExporter struct {
	// GCP-native exporter (used when provider=gcp)
	gcpExporter *GCPExporter

	// Generic OTLP exporter fields
	traceExporter trace.SpanExporter
	grpcClient    coltracepb.TraceServiceClient
	metricClient  colmetricpb.MetricsServiceClient
	logClient     collogspb.LogsServiceClient
	grpcConn      *grpc.ClientConn
	protocol      string
	endpoint      string
}

// NewCloudExporter creates a new cloud exporter.
// When provider=gcp, uses GCP-native APIs (Cloud Trace, Cloud Logging).
// Otherwise, uses standard OTLP gRPC/HTTP forwarding.
func NewCloudExporter(ctx context.Context, config *Config) (*CloudExporter, error) {
	if !config.IsCloudConfigured() {
		return nil, nil
	}

	exporter := &CloudExporter{
		protocol: config.Protocol,
		endpoint: config.Endpoint,
	}

	// Use GCP-native exporters when provider=gcp
	if config.IsGCP() {
		gcpExp, err := NewGCPExporter(config)
		if err != nil {
			return nil, fmt.Errorf("creating GCP exporter: %w", err)
		}
		exporter.gcpExporter = gcpExp
		return exporter, nil
	}

	// Generic OTLP mode
	var err error
	switch config.Protocol {
	case "http":
		err = exporter.initHTTP(ctx, config)
	default:
		err = exporter.initGRPC(ctx, config)
	}

	if err != nil {
		return nil, err
	}

	return exporter, nil
}

// initGRPC initializes the generic OTLP gRPC exporter.
func (e *CloudExporter) initGRPC(ctx context.Context, config *Config) error {
	gcpDialOpts, err := loadSecureGCPDialOptions(ctx, config)
	if err != nil {
		return err
	}

	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(config.Endpoint),
	}
	opts, err = appendOTLPTraceGRPCSecurityOption(opts, config)
	if err != nil {
		return err
	}

	for _, do := range gcpDialOpts {
		opts = append(opts, otlptracegrpc.WithDialOption(do))
	}

	traceExp, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return fmt.Errorf("failed to create gRPC trace exporter: %w", err)
	}

	e.traceExporter = traceExp

	// Also create a raw gRPC client for proto forwarding
	transportCreds, err := otlpGRPCTransportCredentials(config)
	if err != nil {
		return err
	}
	connOpts := []grpc.DialOption{transportCreds}
	connOpts = append(connOpts, gcpDialOpts...)

	conn, err := grpc.NewClient(config.Endpoint, connOpts...)
	if err != nil {
		// Continue without raw client - we can still use SDK exporter
		return nil
	}

	e.grpcConn = conn
	e.grpcClient = coltracepb.NewTraceServiceClient(conn)
	e.metricClient = colmetricpb.NewMetricsServiceClient(conn)
	e.logClient = collogspb.NewLogsServiceClient(conn)

	return nil
}

// initHTTP initializes the generic OTLP HTTP exporter.
func (e *CloudExporter) initHTTP(ctx context.Context, config *Config) error {
	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(config.Endpoint),
	}
	opts, err := appendOTLPTraceHTTPSecurityOption(opts, config)
	if err != nil {
		return err
	}

	traceExp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return fmt.Errorf("failed to create HTTP trace exporter: %w", err)
	}

	e.traceExporter = traceExp
	return nil
}

// ExportSpans exports a batch of SDK spans to the cloud endpoint.
func (e *CloudExporter) ExportSpans(ctx context.Context, spans []trace.ReadOnlySpan) error {
	if e == nil {
		return nil
	}
	if e.gcpExporter != nil {
		return e.gcpExporter.traceExporter.ExportSpans(ctx, spans)
	}
	if e.traceExporter == nil {
		return nil
	}
	return e.traceExporter.ExportSpans(ctx, spans)
}

// ExportProtoSpans exports raw proto spans to the cloud endpoint.
func (e *CloudExporter) ExportProtoSpans(ctx context.Context, resourceSpans []*tracepb.ResourceSpans) error {
	if e == nil {
		return nil
	}

	// GCP-native path: convert proto → SDK → Cloud Trace
	if e.gcpExporter != nil {
		return e.gcpExporter.ExportProtoSpans(ctx, resourceSpans)
	}

	// Generic OTLP path: forward raw proto via gRPC
	if e.grpcClient != nil {
		req := &coltracepb.ExportTraceServiceRequest{
			ResourceSpans: resourceSpans,
		}
		_, err := e.grpcClient.Export(ctx, req)
		return err
	}

	return nil
}

// ExportProtoMetrics exports raw proto metrics to the cloud endpoint.
func (e *CloudExporter) ExportProtoMetrics(ctx context.Context, resourceMetrics []*metricpb.ResourceMetrics) error {
	if e == nil {
		return nil
	}

	// GCP-native path
	if e.gcpExporter != nil {
		return e.gcpExporter.ExportProtoMetrics(ctx, resourceMetrics)
	}

	// Generic OTLP path
	if e.metricClient != nil {
		req := &colmetricpb.ExportMetricsServiceRequest{
			ResourceMetrics: resourceMetrics,
		}
		_, err := e.metricClient.Export(ctx, req)
		return err
	}

	return nil
}

// ExportProtoLogs exports raw proto logs to the cloud endpoint.
func (e *CloudExporter) ExportProtoLogs(ctx context.Context, resourceLogs []*logspb.ResourceLogs) error {
	if e == nil {
		return nil
	}

	// GCP-native path
	if e.gcpExporter != nil {
		return e.gcpExporter.ExportProtoLogs(ctx, resourceLogs)
	}

	// Generic OTLP path
	if e.logClient != nil {
		req := &collogspb.ExportLogsServiceRequest{
			ResourceLogs: resourceLogs,
		}
		_, err := e.logClient.Export(ctx, req)
		return err
	}

	return nil
}

// Shutdown gracefully shuts down the exporter.
func (e *CloudExporter) Shutdown(ctx context.Context) error {
	if e == nil {
		return nil
	}

	var errs []error

	// GCP-native path
	if e.gcpExporter != nil {
		if err := e.gcpExporter.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	// Generic OTLP path
	if e.traceExporter != nil {
		if err := e.traceExporter.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	if e.grpcConn != nil {
		if err := e.grpcConn.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// SpanExporter returns the underlying trace.SpanExporter.
func (e *CloudExporter) SpanExporter() trace.SpanExporter {
	if e == nil {
		return nil
	}
	if e.gcpExporter != nil {
		return e.gcpExporter.traceExporter
	}
	return e.traceExporter
}
