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
	"strings"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
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

// partialSuccessError is terminal: replaying the whole batch duplicates the
// units the destination already accepted.
type partialSuccessError struct{ message string }

func (e *partialSuccessError) Error() string { return e.message }

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
	creds, err := google.CredentialsFromJSONWithType(ctx, keyBytes, google.ServiceAccount,
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
	if config != nil && config.Insecure && (config.SkipTLSVerify || config.CAFile != "") {
		return nil, fmt.Errorf("plaintext OTLP transport conflicts with TLS verification options")
	}
	if config != nil && config.IsGCP() && (config.Insecure || config.SkipTLSVerify || config.CAFile != "") {
		return nil, fmt.Errorf("GCP-native exporter does not support generic OTLP TLS overrides")
	}
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

	// Generic OTLP HTTP has no raw proto forwarding path for all three
	// signals. Reject it before accepting any receiver data.
	if strings.HasPrefix(config.Endpoint, "http://") || strings.HasPrefix(config.Endpoint, "https://") {
		return nil, fmt.Errorf("generic OTLP gRPC endpoint must be a gRPC target without an HTTP URL scheme")
	}
	var err error
	switch config.Protocol {
	case "grpc":
		err = exporter.initGRPC(ctx, config)
	case "http":
		return nil, fmt.Errorf("generic OTLP HTTP export is unsupported")
	default:
		return nil, fmt.Errorf("unsupported OTLP export protocol %q", config.Protocol)
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
		_ = traceExp.Shutdown(ctx)
		return fmt.Errorf("creating OTLP gRPC client: %w", err)
	}

	e.grpcConn = conn
	e.grpcClient = coltracepb.NewTraceServiceClient(conn)
	e.metricClient = colmetricpb.NewMetricsServiceClient(conn)
	e.logClient = collogspb.NewLogsServiceClient(conn)

	return nil
}

// ExportSpans exports a batch of SDK spans to the cloud endpoint.
func (e *CloudExporter) ExportSpans(ctx context.Context, spans []trace.ReadOnlySpan) error {
	if e == nil {
		return errors.New("cloud exporter unavailable")
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
		return errors.New("cloud exporter unavailable")
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
		resp, err := e.grpcClient.Export(ctx, req)
		if err != nil {
			return err
		}
		if partial := resp.GetPartialSuccess(); partial != nil && partial.GetRejectedSpans() != 0 {
			return &partialSuccessError{fmt.Sprintf("OTLP trace partial success: %d rejected spans: %s", partial.GetRejectedSpans(), partial.GetErrorMessage())}
		}
		return nil
	}

	return errors.New("OTLP trace client unavailable")
}

// ExportProtoMetrics exports raw proto metrics to the cloud endpoint.
func (e *CloudExporter) ExportProtoMetrics(ctx context.Context, resourceMetrics []*metricpb.ResourceMetrics) error {
	if e == nil {
		return errors.New("cloud exporter unavailable")
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
		resp, err := e.metricClient.Export(ctx, req)
		if err != nil {
			return err
		}
		if partial := resp.GetPartialSuccess(); partial != nil && partial.GetRejectedDataPoints() != 0 {
			return &partialSuccessError{fmt.Sprintf("OTLP metric partial success: %d rejected points: %s", partial.GetRejectedDataPoints(), partial.GetErrorMessage())}
		}
		return nil
	}

	return errors.New("OTLP metric client unavailable")
}

// ExportProtoLogs exports raw proto logs to the cloud endpoint.
func (e *CloudExporter) ExportProtoLogs(ctx context.Context, resourceLogs []*logspb.ResourceLogs) error {
	if e == nil {
		return errors.New("cloud exporter unavailable")
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
		resp, err := e.logClient.Export(ctx, req)
		if err != nil {
			return err
		}
		if partial := resp.GetPartialSuccess(); partial != nil && partial.GetRejectedLogRecords() != 0 {
			return &partialSuccessError{fmt.Sprintf("OTLP log partial success: %d rejected records: %s", partial.GetRejectedLogRecords(), partial.GetErrorMessage())}
		}
		return nil
	}

	return errors.New("OTLP log client unavailable")
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
