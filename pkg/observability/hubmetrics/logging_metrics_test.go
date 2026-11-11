/*
Copyright 2026 The Scion Authors.
*/

package hubmetrics

import (
	"context"
	"testing"

	mexporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric"
	"google.golang.org/api/option"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/GoogleCloudPlatform/scion/pkg/util/asyncwrite"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// loggingWriterSource is a fixed async writer source with every counter
// nonzero, so each scion.logging.* series is exported.
type loggingWriterSource struct{}

func (loggingWriterSource) Name() string      { return "audit" }
func (loggingWriterSource) QueueDepth() int64 { return 1 }
func (loggingWriterSource) Stalled() bool     { return true }
func (loggingWriterSource) Snapshot() asyncwrite.Snapshot {
	return asyncwrite.Snapshot{Written: 1, LateReturns: 1, WriteErrors: 1, DroppedFull: 1}
}

// loggingMetricNames are the OTel names logging.WriteMetrics registers.
var loggingMetricNames = map[string]instrumentKind{
	logging.MetricWriteFailures:     kindInt64Counter,
	logging.MetricWriteRecords:      kindInt64Counter,
	logging.MetricWriteLateReturns:  kindInt64Counter,
	logging.MetricQueueDepth:        kindInt64Gauge,
	logging.MetricWriterStalled:     kindInt64Gauge,
	logging.MetricWriterCircuitOpen: kindInt64Gauge,
}

// exportLoggingThroughFakeAPI exports the real logging.WriteMetrics
// instruments (audit and cloud writers) through the production
// NewMeterProvider to the fake Cloud Monitoring API and returns what
// arrived keyed by metric type. The instruments are observable, so they are
// driven by their sources instead of replayed like hubRecorderInstruments.
func exportLoggingThroughFakeAPI(t *testing.T) map[string]exportedMetric {
	t.Helper()
	ctx := context.Background()
	fake, addr := startFakeMonitoringAPI(t)
	mp, err := NewMeterProvider(ctx, "logging-test",
		WithHubID("hub-a"), WithInstanceID("replica-1"),
		withExporterOptions(mexporter.WithMonitoringClientOptions(
			option.WithEndpoint(addr),
			option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
		)))
	if err != nil {
		t.Fatalf("NewMeterProvider: %v", err)
	}
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	wm, err := logging.NewWriteMetrics(mp)
	if err != nil {
		t.Fatalf("NewWriteMetrics: %v", err)
	}
	wm.Observe(loggingWriterSource{})
	cloud := &logging.CloudWriteStats{}
	cloud.RecordFailure(logging.CloudReasonCircuitOpen)
	cloud.RegisterCircuitSource(func() bool { return true })
	wm.ObserveCloud(cloud)
	if err := mp.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	out := map[string]exportedMetric{}
	for _, m := range collectExported(t, fake, loggingMetricNames) {
		out["workload.googleapis.com/"+m.otelName] = m
	}
	return out
}

// The scion.logging.* names reach Cloud Monitoring with their kinds
// unchanged by the switch to observable counters (R6-Q3): counters stay
// CUMULATIVE INT64, gauges stay GAUGE INT64.
func TestLoggingMetricsCloudMonitoringKinds(t *testing.T) {
	hermeticMetricsEnv(t)
	exported := exportLoggingThroughFakeAPI(t)
	for name, want := range map[string]metricpb.MetricDescriptor_MetricKind{
		logging.MetricWriteFailures:     metricpb.MetricDescriptor_CUMULATIVE,
		logging.MetricWriteRecords:      metricpb.MetricDescriptor_CUMULATIVE,
		logging.MetricWriteLateReturns:  metricpb.MetricDescriptor_CUMULATIVE,
		logging.MetricQueueDepth:        metricpb.MetricDescriptor_GAUGE,
		logging.MetricWriterStalled:     metricpb.MetricDescriptor_GAUGE,
		logging.MetricWriterCircuitOpen: metricpb.MetricDescriptor_GAUGE,
	} {
		m, ok := exported["workload.googleapis.com/"+name]
		if !ok {
			t.Errorf("%s not exported (got %v)", name, exported)
			continue
		}
		if m.kind != want || m.valueType != metricpb.MetricDescriptor_INT64 {
			t.Errorf("%s: kind %v value type %v, want %v INT64", name, m.kind, m.valueType, want)
		}
		if m.labels["writer"] == "" {
			t.Errorf("%s: writer label missing: %v", name, m.labels)
		}
	}
}
