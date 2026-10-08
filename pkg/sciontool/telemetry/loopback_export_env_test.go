/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"net"
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
// OTLP headers, compression and timeout variables in sciontool's environment
// must not change how its loopback providers export, for any signal.
func TestLoopbackProvidersIgnoreOTLPExporterEnv(t *testing.T) {
	for _, signal := range []string{"", "TRACES_", "METRICS_", "LOGS_"} {
		t.Setenv("OTEL_EXPORTER_OTLP_"+signal+"HEADERS", "x-ambient-header=leak")
		t.Setenv("OTEL_EXPORTER_OTLP_"+signal+"COMPRESSION", "gzip")
		t.Setenv("OTEL_EXPORTER_OTLP_"+signal+"TIMEOUT", "1")
	}

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
			// A 1ms ambient timeout would make these exports fail.
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
