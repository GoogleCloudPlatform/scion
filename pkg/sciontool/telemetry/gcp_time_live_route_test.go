package telemetry

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	mexporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/api/option"
	googlemetricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type localCadencePoint struct {
	Metric     string
	Resource   string
	Identity   string
	Start, End time.Time
	Value      int64
	Accepted   bool
	Code       codes.Code
}
type localCadenceMonitoring struct {
	monitoringpb.UnimplementedMetricServiceServer
	mu     sync.Mutex
	last   map[string]time.Time
	points []localCadencePoint
}

func (*localCadenceMonitoring) GetMetricDescriptor(context.Context, *monitoringpb.GetMetricDescriptorRequest) (*googlemetricpb.MetricDescriptor, error) {
	return nil, status.Error(codes.NotFound, "local missing descriptor")
}
func (*localCadenceMonitoring) CreateMetricDescriptor(_ context.Context, r *monitoringpb.CreateMetricDescriptorRequest) (*googlemetricpb.MetricDescriptor, error) {
	return r.MetricDescriptor, nil
}
func (m *localCadenceMonitoring) CreateTimeSeries(_ context.Context, r *monitoringpb.CreateTimeSeriesRequest) (*emptypb.Empty, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ts := range r.TimeSeries {
		if !strings.Contains(ts.Metric.Type, "agent.tool.calls") && !strings.Contains(ts.Metric.Type, "scion.test") {
			continue
		}
		point := ts.Points[0]
		start := point.Interval.StartTime.AsTime()
		end := point.Interval.EndTime.AsTime()
		key := fmt.Sprintf("%s|%s|%v|%v", ts.Metric.Type, ts.Resource.Type, ts.Metric.Labels, ts.Resource.Labels)
		cp := localCadencePoint{Metric: ts.Metric.Type, Resource: ts.Resource.Type, Identity: key, Start: start, End: end, Value: point.Value.GetInt64Value(), Accepted: true, Code: codes.OK}
		if prior, ok := m.last[key]; ok && end.Sub(prior) < 5*time.Second {
			cp.Accepted = false
			cp.Code = codes.InvalidArgument
			m.points = append(m.points, cp)
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("local same-series end gap %v below 5s", end.Sub(prior)))
		}
		m.last[key] = end
		m.points = append(m.points, cp)
	}
	return &emptypb.Empty{}, nil
}

func TestGCPHookProviderCloudPointCadence(t *testing.T) {
	for _, firstAfter := range []int{2, 10} {
		t.Run(fmt.Sprintf("first_after_%d", firstAfter), func(t *testing.T) {
			t.Setenv("SCION_AGENT_ID", "local-cadence-agent")
			t.Setenv("SCION_PROJECT_ID", "local-cadence-project")
			t.Setenv("SCION_HARNESS", "synthetic")
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			fake := &localCadenceMonitoring{last: map[string]time.Time{}}
			server := grpc.NewServer()
			monitoringpb.RegisterMetricServiceServer(server, fake)
			go server.Serve(listener)
			defer server.Stop()
			conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			sdk, err := mexporter.New(mexporter.WithProjectID("test-project"), mexporter.WithMonitoringClientOptions(option.WithGRPCConn(conn)))
			if err != nil {
				t.Fatal(err)
			}
			defer sdk.Shutdown(context.Background())
			cfg := &Config{Enabled: true, CloudProvider: "gcp", GRPCPort: availableTCPPort(t)}
			p := NewWithConfig(cfg)
			p.exporter = &CloudExporter{gcpExporter: &GCPExporter{metricExporter: sdk}}
			receiver := NewReceiver(cfg, nil, WithMetricHandler(p.handleMetrics))
			if err := receiver.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer receiver.Stop(context.Background())
			for i := 0; i < 10; i++ {
				cmd := exec.Command(os.Args[0], "-test.run=^TestMetricHookChild$", "-test.count=1")
				cmd.Env = append(os.Environ(), fmt.Sprintf("SCION_TEST_HOOK_PORT=%d", cfg.GRPCPort), "SCION_AGENT_ID=local-cadence-agent", "SCION_PROJECT_ID=local-cadence-project", "SCION_HARNESS=synthetic")
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("hook %d: %v %s", i, err, output)
				}
				if i+1 == firstAfter {
					if firstAfter == 10 {
						t.Log("old initial 15s tick control: delaying first export 15.1s after hooks")
						time.Sleep(15*time.Second + 100*time.Millisecond)
					}
					if !p.flushMetricBuffer(context.Background(), true) {
						t.Fatal("first export not confirmed")
					}
					t.Logf("first invocation completed=%s", time.Now().UTC().Format(time.RFC3339Nano))
				}
			}
			if firstAfter < 10 {
				t.Logf("second invocation waiting 15.1s after first completion, current=%s", time.Now().UTC().Format(time.RFC3339Nano))
				time.Sleep(15*time.Second + 100*time.Millisecond)
				ok := p.flushMetricBuffer(context.Background(), true)
				t.Logf("second pipeline confirmed=%v error=%v", ok, !ok)
				if !ok {
					t.Fatal("second export not confirmed")
				}
				for i := 0; i < 2; i++ {
					cmd := exec.Command(os.Args[0], "-test.run=^TestMetricHookChild$", "-test.count=1")
					cmd.Env = append(os.Environ(), fmt.Sprintf("SCION_TEST_HOOK_PORT=%d", cfg.GRPCPort), "SCION_AGENT_ID=local-cadence-agent", "SCION_PROJECT_ID=local-cadence-project", "SCION_HARNESS=synthetic")
					if output, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("later hook %d: %v %s", i, err, output)
					}
				}
				time.Sleep(15*time.Second + 100*time.Millisecond)
				if !p.flushMetricBuffer(context.Background(), true) {
					t.Fatal("later-boundary burst not confirmed")
				}
			}
			fake.mu.Lock()
			points := append([]localCadencePoint(nil), fake.points...)
			fake.mu.Unlock()
			for i, pt := range points {
				t.Logf("Cloud point %d metric=%s resource=%s identity=%s start=%s end=%s value=%d accepted=%v code=%v", i+1, pt.Metric, pt.Resource, pt.Identity, pt.Start.UTC().Format(time.RFC3339Nano), pt.End.UTC().Format(time.RFC3339Nano), pt.Value, pt.Accepted, pt.Code)
			}
			if firstAfter == 2 {
				if len(points) != 3 || points[0].Value != 2 || points[1].Value != 10 || points[2].Value != 12 || !points[1].Accepted || !points[2].Accepted || points[1].Code != codes.OK || points[2].Code != codes.OK || !points[0].Start.Equal(points[1].Start) || !points[1].Start.Equal(points[2].Start) {
					t.Fatalf("unexpected split points %+v", points)
				}
			} else {
				if len(points) != 1 || points[0].Value != 10 || !points[0].Accepted {
					t.Fatalf("unexpected delayed point %+v", points)
				}
			}
		})
	}
}

// Synthetic native timestamps exercise the actual adapter and pinned SDK locally.
func TestGCPNativePointEndBoundary(t *testing.T) {
	for _, kind := range []string{"sum", "histogram", "gauge"} {
		t.Run(kind, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			fake := &localCadenceMonitoring{last: map[string]time.Time{}}
			server := grpc.NewServer()
			monitoringpb.RegisterMetricServiceServer(server, fake)
			go server.Serve(listener)
			defer server.Stop()
			conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			sdk, err := mexporter.New(mexporter.WithProjectID("test-project"), mexporter.WithMonitoringClientOptions(option.WithGRPCConn(conn)))
			if err != nil {
				t.Fatal(err)
			}
			defer sdk.Shutdown(context.Background())
			exporter := &GCPExporter{metricExporter: sdk}
			start := uint64(time.Now().Add(-time.Minute).UnixNano())
			first := start + uint64(10*time.Second)
			makeMetric := func(end uint64) *metricpb.Metric {
				switch kind {
				case "sum":
					return testNumber("scion.test.sum", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, start, end, 7)
				case "histogram":
					m := testHist(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, start, end, 7, 7, []float64{10}, []uint64{7, 0})
					m.Name = "scion.test.histogram"
					return m
				default:
					return &metricpb.Metric{Name: "scion.test.gauge", Data: &metricpb.Metric_Gauge{Gauge: &metricpb.Gauge{DataPoints: []*metricpb.NumberDataPoint{{TimeUnixNano: end, Value: &metricpb.NumberDataPoint_AsInt{AsInt: 7}}}}}}
				}
			}
			for i, end := range []uint64{first, first + uint64(4*time.Second), first + uint64(5*time.Second)} {
				input := testMetricResource("sciontool", "native.scope", "1", "native", makeMetric(end))
				err := exporter.ExportProtoMetrics(context.Background(), []*metricpb.ResourceMetrics{input})
				t.Logf("native %s point %d end_offset=%s SDK_error=%v code=%v", kind, i, time.Duration(end-first), err, status.Code(err))
				if i == 1 && status.Code(err) != codes.InvalidArgument {
					t.Fatalf("4s point expected InvalidArgument, got %v", err)
				}
				if i != 1 && err != nil {
					t.Fatalf("point %d unexpected error %v", i, err)
				}
			}
			fake.mu.Lock()
			points := append([]localCadencePoint(nil), fake.points...)
			fake.mu.Unlock()
			if len(points) != 3 {
				t.Fatalf("captured %d points: %+v", len(points), points)
			}
			for i, pt := range points {
				t.Logf("native %s mapped %d identity=%s start=%s end=%s accepted=%v code=%v", kind, i, pt.Identity, pt.Start.UTC().Format(time.RFC3339Nano), pt.End.UTC().Format(time.RFC3339Nano), pt.Accepted, pt.Code)
			}
		})
	}
}

func TestPairedHookCounterHistogramOneRequest(t *testing.T) {
	cfg := &Config{Enabled: true, CloudProvider: "gcp", GRPCPort: availableTCPPort(t)}
	type captured struct {
		names []string
		ends  []uint64
	}
	got := make(chan captured, 2)
	receiver := NewReceiver(cfg, nil, WithMetricHandler(func(_ context.Context, rms []*metricpb.ResourceMetrics) error {
		var c captured
		for _, rm := range rms {
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					c.names = append(c.names, m.Name)
					if m.Name == "agent.tool.calls" {
						for _, pt := range m.GetSum().DataPoints {
							c.ends = append(c.ends, pt.TimeUnixNano)
						}
					}
					if m.Name == "agent.tool.duration" {
						for _, pt := range m.GetHistogram().DataPoints {
							c.ends = append(c.ends, pt.TimeUnixNano)
						}
					}
				}
			}
		}
		got <- c
		return nil
	}))
	if err := receiver.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer receiver.Stop(context.Background())
	for i := 0; i < 2; i++ {
		p, err := NewProviders(context.Background(), cfg, false)
		if err != nil {
			t.Fatal(err)
		}
		meter := p.MeterProvider.Meter(hookMetricScope)
		counter, err := meter.Int64Counter("agent.tool.calls", otelmetric.WithUnit("{call}"))
		if err != nil {
			t.Fatal(err)
		}
		hist, err := meter.Float64Histogram("agent.tool.duration", otelmetric.WithUnit("ms"))
		if err != nil {
			t.Fatal(err)
		}
		attrs := otelmetric.WithAttributes(attribute.String("tool_name", "local-tool"), attribute.String("status", "success"))
		counter.Add(context.Background(), 1, attrs)
		hist.Record(context.Background(), float64(i+1), attrs)
		if err := p.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		select {
		case c := <-got:
			t.Logf("paired request %d names=%v ends=%v", i, c.names, c.ends)
			if len(c.names) != 2 || len(c.ends) != 2 {
				t.Fatalf("not paired in one Export request: %+v", c)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no paired export")
		}
	}
}

func TestGCPNativeAdmissionUsesPinnedSDKMappedEnd(t *testing.T) {
	for _, kind := range []string{"sum", "histogram", "gauge"} {
		for _, duration := range []time.Duration{500 * time.Microsecond, time.Millisecond, 2*time.Millisecond - time.Nanosecond, 2 * time.Millisecond} {
			t.Run(fmt.Sprintf("%s/%s", kind, duration), func(t *testing.T) {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				fake := &localCadenceMonitoring{last: map[string]time.Time{}}
				server := grpc.NewServer()
				monitoringpb.RegisterMetricServiceServer(server, fake)
				go server.Serve(listener)
				defer server.Stop()
				conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				sdk, err := mexporter.New(mexporter.WithProjectID("test-project"), mexporter.WithMonitoringClientOptions(option.WithGRPCConn(conn)))
				if err != nil {
					t.Fatal(err)
				}
				defer sdk.Shutdown(context.Background())
				p := NewWithConfig(&Config{Enabled: true, CloudProvider: "gcp"})
				p.exporter = &CloudExporter{gcpExporter: &GCPExporter{metricExporter: sdk}}
				start := uint64(time.Unix(1000, 0).UnixNano())
				makePoint := func(end uint64) *metricpb.ResourceMetrics {
					var metric *metricpb.Metric
					switch kind {
					case "sum":
						metric = testNumber("scion.test.sum", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, start, end, int64(end))
					case "histogram":
						metric = testHist(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, start, end, 1, 1, []float64{10}, []uint64{1, 0})
						metric.Name = "scion.test.histogram"
					default:
						metric = &metricpb.Metric{Name: "scion.test.gauge", Data: &metricpb.Metric_Gauge{Gauge: &metricpb.Gauge{DataPoints: []*metricpb.NumberDataPoint{{TimeUnixNano: end, Value: &metricpb.NumberDataPoint_AsInt{AsInt: int64(end)}}}}}}
					}
					return testMetricResource("sciontool", "native.scope", "1", "native", metric)
				}
				firstEnd := start + uint64(duration)
				if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{makePoint(firstEnd)}); err != nil {
					t.Fatal(err)
				}
				if !p.flushMetricBuffer(context.Background(), true) {
					t.Fatal("first export")
				}
				fake.mu.Lock()
				if len(fake.points) != 1 {
					t.Fatalf("first mapped points=%d", len(fake.points))
				}
				mapped := fake.points[0].End
				fake.mu.Unlock()
				expect := time.Unix(1000, 0).Add(duration)
				if kind != "gauge" && duration < 2*time.Millisecond {
					expect = time.Unix(1000, 0).Add(time.Millisecond)
				}
				if !mapped.Equal(expect) {
					t.Fatalf("mapped=%s expected=%s", mapped, expect)
				}
				below := uint64(mapped.Add(5*time.Second - time.Nanosecond).UnixNano())
				if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{makePoint(below)}); status.Code(err) != codes.InvalidArgument {
					t.Fatalf("below mapped boundary: %v", err)
				}
				exact := uint64(mapped.Add(5 * time.Second).UnixNano())
				if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{makePoint(exact)}); err != nil {
					t.Fatalf("exact mapped boundary: %v", err)
				}
				if !p.flushMetricBuffer(context.Background(), true) {
					t.Fatal("second export")
				}
				fake.mu.Lock()
				count := len(fake.points)
				fake.mu.Unlock()
				if count != 2 {
					t.Fatalf("Cloud point count=%d", count)
				}
			})
		}
	}
}
