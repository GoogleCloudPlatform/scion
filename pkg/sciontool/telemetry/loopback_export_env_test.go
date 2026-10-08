/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
	"google.golang.org/protobuf/types/known/emptypb"
)

// loopbackCall is what a stub OTLP server saw for one export.
type loopbackCall struct {
	method      string
	compression string
	header      metadata.MD
	remaining   time.Duration
}

type loopbackCallRecorder struct {
	mu    sync.Mutex
	calls []loopbackCall
}

type compressionKey struct{}

// TagRPC/HandleRPC record the grpc-encoding the client sent, which incoming
// metadata does not expose.
func (r *loopbackCallRecorder) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return context.WithValue(ctx, compressionKey{}, new(string))
}

func (r *loopbackCallRecorder) HandleRPC(ctx context.Context, s stats.RPCStats) {
	if in, ok := s.(*stats.InHeader); ok {
		if p, ok := ctx.Value(compressionKey{}).(*string); ok {
			*p = in.Compression
		}
	}
}

func (r *loopbackCallRecorder) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (r *loopbackCallRecorder) HandleConn(context.Context, stats.ConnStats) {}

func (r *loopbackCallRecorder) handle(_ any, stream grpc.ServerStream) error {
	ctx := stream.Context()
	call := loopbackCall{}
	call.method, _ = grpc.MethodFromServerStream(stream)
	call.header, _ = metadata.FromIncomingContext(ctx)
	if p, ok := ctx.Value(compressionKey{}).(*string); ok {
		call.compression = *p
	}
	if deadline, ok := ctx.Deadline(); ok {
		call.remaining = time.Until(deadline)
	}
	// The request is decoded into Empty (its fields are kept as unknown
	// fields) and an empty response is a valid Export*ServiceResponse.
	if err := stream.RecvMsg(&emptypb.Empty{}); err != nil {
		return err
	}
	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()
	return stream.SendMsg(&emptypb.Empty{})
}

func startLoopbackCallRecorder(t *testing.T) (*loopbackCallRecorder, int) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rec := &loopbackCallRecorder{}
	srv := grpc.NewServer(grpc.StatsHandler(rec), grpc.UnknownServiceHandler(rec.handle))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return rec, lis.Addr().(*net.TCPAddr).Port
}

// TestLoopbackProvidersIgnoreOTLPExporterEnv pins ptone/scion#2992: the
// OTLP headers, compression, timeout and TLS certificate variables in
// sciontool's environment must not change how its loopback providers
// export, for any signal.
func TestLoopbackProvidersIgnoreOTLPExporterEnv(t *testing.T) {
	certFile, keyFile := writeTestCertificate(t)
	for _, env := range []struct {
		name string
		vars map[string]string
	}{
		{
			name: "headers compression timeout",
			vars: map[string]string{"HEADERS": "x-ambient-header=leak", "COMPRESSION": "gzip", "TIMEOUT": "1"},
		},
		{
			name: "certificate",
			vars: map[string]string{"CERTIFICATE": certFile},
		},
		{
			name: "client certificate",
			vars: map[string]string{"CLIENT_CERTIFICATE": certFile, "CLIENT_KEY": keyFile},
		},
	} {
		t.Run(env.name, func(t *testing.T) {
			for _, signal := range []string{"", "TRACES_", "METRICS_", "LOGS_"} {
				for key, value := range env.vars {
					t.Setenv("OTEL_EXPORTER_OTLP_"+signal+key, value)
				}
			}
			testLoopbackProvidersExportUnchanged(t)
		})
	}
}

// writeTestCertificate writes a self-signed certificate and its key as PEM
// files and returns their paths.
func writeTestCertificate(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "loopback-env-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// testLoopbackProvidersExportUnchanged exports one span, log record and
// metric through each loopback provider kind and checks that every signal
// arrives uncompressed, with no extra headers and the pinned deadline.
func testLoopbackProvidersExportUnchanged(t *testing.T) {
	t.Helper()
	for _, tc := range []struct {
		name    string
		new     func(context.Context, *Config) (*Providers, error)
		timeout time.Duration
	}{
		{
			name:    "providers",
			new:     func(ctx context.Context, cfg *Config) (*Providers, error) { return NewProviders(ctx, cfg, false) },
			timeout: LoopbackExportTimeout,
		},
		{
			name:    "batch providers",
			new:     func(ctx context.Context, cfg *Config) (*Providers, error) { return NewProviders(ctx, cfg, true) },
			timeout: LoopbackExportTimeout,
		},
		{
			name:    "hook providers",
			new:     NewHookProviders,
			timeout: HookExportTimeout,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, port := startLoopbackCallRecorder(t)
			ctx := context.Background()
			providers, err := tc.new(ctx, &Config{Enabled: true, GRPCPort: port})
			if err != nil {
				t.Fatal(err)
			}
			_, span := providers.TracerProvider.Tracer("env.test").Start(ctx, "event")
			span.End()
			var record otellog.Record
			record.SetEventName("event")
			providers.LoggerProvider.Logger("env.test").Emit(ctx, record)
			counter, err := providers.MeterProvider.Meter("env.test").Int64Counter("env.counter")
			if err != nil {
				t.Fatal(err)
			}
			counter.Add(ctx, 1)
			shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			// A 1ms ambient timeout, or TLS credentials from the
			// environment, would make these exports fail.
			if err := providers.Shutdown(shutdownCtx); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}

			rec.mu.Lock()
			defer rec.mu.Unlock()
			seen := map[string]bool{}
			for _, call := range rec.calls {
				for _, signal := range []string{"trace", "metrics", "logs"} {
					if strings.Contains(call.method, "collector."+signal+".") {
						seen[signal] = true
					}
				}
				if got := call.header.Get("x-ambient-header"); len(got) != 0 {
					t.Errorf("%s: ambient header reached the receiver: %v", call.method, got)
				}
				if call.compression != "" && call.compression != "identity" {
					t.Errorf("%s: compression = %q, want none", call.method, call.compression)
				}
				if call.remaining <= tc.timeout/10 || call.remaining > tc.timeout {
					t.Errorf("%s: deadline in %v, want about %v", call.method, call.remaining, tc.timeout)
				}
			}
			for _, signal := range []string{"trace", "metrics", "logs"} {
				if !seen[signal] {
					t.Errorf("no %s export reached the receiver; calls: %+v", signal, rec.calls)
				}
			}
		})
	}
}
