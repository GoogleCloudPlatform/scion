/*
Copyright 2026 The Scion Authors.
*/

package hubmetrics

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	mexporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"google.golang.org/api/option"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/GoogleCloudPlatform/scion/pkg/observability/dbmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/dispatchmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/reapermetrics"
)

// hubDashboardPath is the importable Cloud Monitoring dashboard for the hub.
var hubDashboardPath = filepath.Join("..", "..", "..", "deploy", "monitoring", "dashboards", "scion-hub.json")

// pointAttributeLabels are the per-point attribute keys a dashboard chart may
// filter or group by in addition to the resource-derived labels. Callers set
// them at record time, so they come from the constants the call sites use.
var pointAttributeLabels = map[string]map[string]bool{
	reapermetrics.MetricLaunchReaperTicks: {reapermetrics.AttrTickOutcome: true},
	dbmetrics.MetricNotificationsDropped:  {dbmetrics.AttrDropReason: true},
}

// --- instrument discovery --------------------------------------------------

type instrumentKind int

const (
	kindInt64Counter instrumentKind = iota
	kindFloat64Counter
	kindInt64UpDownCounter
	kindFloat64UpDownCounter
	kindInt64Histogram
	kindFloat64Histogram
	kindInt64Gauge
	kindFloat64Gauge
)

// capturingMeterProvider records every instrument a recorder registers, so
// the test derives the hub's metric set from the recorder constructors rather
// than from a hand-copied list. Instruments are backed by no-ops.
type capturingMeterProvider struct {
	noop.MeterProvider
	mu          sync.Mutex
	instruments map[string]instrumentKind
	unsupported []string
}

func (p *capturingMeterProvider) Meter(string, ...otelmetric.MeterOption) otelmetric.Meter {
	return &capturingMeter{p: p}
}

func (p *capturingMeterProvider) add(name string, k instrumentKind) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.instruments[name] = k
}

func (p *capturingMeterProvider) addUnsupported(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unsupported = append(p.unsupported, name)
}

type capturingMeter struct {
	noop.Meter
	p *capturingMeterProvider
}

func (m *capturingMeter) Int64Counter(n string, o ...otelmetric.Int64CounterOption) (otelmetric.Int64Counter, error) {
	m.p.add(n, kindInt64Counter)
	return m.Meter.Int64Counter(n, o...)
}

func (m *capturingMeter) Float64Counter(n string, o ...otelmetric.Float64CounterOption) (otelmetric.Float64Counter, error) {
	m.p.add(n, kindFloat64Counter)
	return m.Meter.Float64Counter(n, o...)
}

func (m *capturingMeter) Int64UpDownCounter(n string, o ...otelmetric.Int64UpDownCounterOption) (otelmetric.Int64UpDownCounter, error) {
	m.p.add(n, kindInt64UpDownCounter)
	return m.Meter.Int64UpDownCounter(n, o...)
}

func (m *capturingMeter) Float64UpDownCounter(n string, o ...otelmetric.Float64UpDownCounterOption) (otelmetric.Float64UpDownCounter, error) {
	m.p.add(n, kindFloat64UpDownCounter)
	return m.Meter.Float64UpDownCounter(n, o...)
}

func (m *capturingMeter) Int64Histogram(n string, o ...otelmetric.Int64HistogramOption) (otelmetric.Int64Histogram, error) {
	m.p.add(n, kindInt64Histogram)
	return m.Meter.Int64Histogram(n, o...)
}

func (m *capturingMeter) Float64Histogram(n string, o ...otelmetric.Float64HistogramOption) (otelmetric.Float64Histogram, error) {
	m.p.add(n, kindFloat64Histogram)
	return m.Meter.Float64Histogram(n, o...)
}

func (m *capturingMeter) Int64Gauge(n string, o ...otelmetric.Int64GaugeOption) (otelmetric.Int64Gauge, error) {
	m.p.add(n, kindInt64Gauge)
	return m.Meter.Int64Gauge(n, o...)
}

func (m *capturingMeter) Float64Gauge(n string, o ...otelmetric.Float64GaugeOption) (otelmetric.Float64Gauge, error) {
	m.p.add(n, kindFloat64Gauge)
	return m.Meter.Float64Gauge(n, o...)
}

// Observable instruments are not replayed below. Record them so the test
// fails loudly if one of the charted recorders starts using them.
func (m *capturingMeter) Int64ObservableCounter(n string, o ...otelmetric.Int64ObservableCounterOption) (otelmetric.Int64ObservableCounter, error) {
	m.p.addUnsupported(n)
	return m.Meter.Int64ObservableCounter(n, o...)
}

func (m *capturingMeter) Float64ObservableCounter(n string, o ...otelmetric.Float64ObservableCounterOption) (otelmetric.Float64ObservableCounter, error) {
	m.p.addUnsupported(n)
	return m.Meter.Float64ObservableCounter(n, o...)
}

func (m *capturingMeter) Int64ObservableUpDownCounter(n string, o ...otelmetric.Int64ObservableUpDownCounterOption) (otelmetric.Int64ObservableUpDownCounter, error) {
	m.p.addUnsupported(n)
	return m.Meter.Int64ObservableUpDownCounter(n, o...)
}

func (m *capturingMeter) Float64ObservableUpDownCounter(n string, o ...otelmetric.Float64ObservableUpDownCounterOption) (otelmetric.Float64ObservableUpDownCounter, error) {
	m.p.addUnsupported(n)
	return m.Meter.Float64ObservableUpDownCounter(n, o...)
}

func (m *capturingMeter) Int64ObservableGauge(n string, o ...otelmetric.Int64ObservableGaugeOption) (otelmetric.Int64ObservableGauge, error) {
	m.p.addUnsupported(n)
	return m.Meter.Int64ObservableGauge(n, o...)
}

func (m *capturingMeter) Float64ObservableGauge(n string, o ...otelmetric.Float64ObservableGaugeOption) (otelmetric.Float64ObservableGauge, error) {
	m.p.addUnsupported(n)
	return m.Meter.Float64ObservableGauge(n, o...)
}

// hubRecorderInstruments returns every instrument registered by the hub
// recorders that cmd/server_foreground.go wires to the Cloud Monitoring
// MeterProvider (wireHubCoreMetrics) and that the dashboard charts.
func hubRecorderInstruments(t *testing.T) map[string]instrumentKind {
	t.Helper()
	p := &capturingMeterProvider{instruments: map[string]instrumentKind{}}
	if _, err := dbmetrics.New(p); err != nil {
		t.Fatalf("dbmetrics.New: %v", err)
	}
	if _, err := dispatchmetrics.New(p); err != nil {
		t.Fatalf("dispatchmetrics.New: %v", err)
	}
	if _, err := reapermetrics.New(p); err != nil {
		t.Fatalf("reapermetrics.New: %v", err)
	}
	if len(p.unsupported) > 0 {
		t.Fatalf("observable instruments are not handled by this test: %v", p.unsupported)
	}
	if len(p.instruments) == 0 {
		t.Fatal("no instruments captured from hub recorders")
	}
	return p.instruments
}

// --- fake Cloud Monitoring API ---------------------------------------------

type fakeMetricService struct {
	monitoringpb.UnimplementedMetricServiceServer
	mu          sync.Mutex
	descriptors map[string]*metricpb.MetricDescriptor
	series      []*monitoringpb.TimeSeries
}

func (f *fakeMetricService) CreateMetricDescriptor(_ context.Context, req *monitoringpb.CreateMetricDescriptorRequest) (*metricpb.MetricDescriptor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.descriptors[req.GetMetricDescriptor().GetType()] = req.GetMetricDescriptor()
	return req.GetMetricDescriptor(), nil
}

func (f *fakeMetricService) CreateTimeSeries(_ context.Context, req *monitoringpb.CreateTimeSeriesRequest) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.series = append(f.series, req.GetTimeSeries()...)
	return &emptypb.Empty{}, nil
}

// exportedMetric is what Cloud Monitoring receives for one hub metric from
// one replica.
type exportedMetric struct {
	otelName     string
	kind         metricpb.MetricDescriptor_MetricKind
	valueType    metricpb.MetricDescriptor_ValueType
	labels       map[string]string
	resourceType string
	resource     map[string]string
}

// hermeticMetricsEnv clears the environment variables NewMeterProvider reads,
// so a value leaked from the caller's shell (for example
// SCION_METRICS_DISPATCH=false) cannot drop instruments or change the hub ID.
func hermeticMetricsEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SCION_HUB_ID", "")
	for _, g := range metricGroups {
		t.Setenv(g.EnvVar, "")
	}
}

// startFakeMonitoringAPI serves an in-process fake Cloud Monitoring metric
// API on a loopback port and returns it with its address.
func startFakeMonitoringAPI(t *testing.T) (*fakeMetricService, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fake := &fakeMetricService{descriptors: map[string]*metricpb.MetricDescriptor{}}
	srv := grpc.NewServer()
	monitoringpb.RegisterMetricServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return fake, lis.Addr().String()
}

// exportReplica records one point on every hub instrument through the
// production NewMeterProvider (exporter, resource attributes and label filter
// included) for a hub replica with the given hub ID, and flushes it to the
// fake API at addr. extra holds further options, usually WithInstanceID;
// callers that leave it out exercise the generated-ID fallback.
func exportReplica(t *testing.T, addr, hubID string, instruments map[string]instrumentKind, extra ...Option) {
	t.Helper()
	ctx := context.Background()
	opts := []Option{
		WithHubID(hubID),
		WithHubName("dashboard test hub"),
		withExporterOptions(mexporter.WithMonitoringClientOptions(
			option.WithEndpoint(addr),
			option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
		)),
	}
	opts = append(opts, extra...)
	mp, err := NewMeterProvider(ctx, "dashboard-test", opts...)
	if err != nil {
		t.Fatalf("NewMeterProvider: %v", err)
	}
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	meter := mp.Meter("dashboard-test")
	attrs := otelmetric.WithAttributes(attribute.String("dashboard_test", "1"))
	for name, k := range instruments {
		var err error
		switch k {
		case kindInt64Counter:
			var i otelmetric.Int64Counter
			if i, err = meter.Int64Counter(name); err == nil {
				i.Add(ctx, 1, attrs)
			}
		case kindFloat64Counter:
			var i otelmetric.Float64Counter
			if i, err = meter.Float64Counter(name); err == nil {
				i.Add(ctx, 1, attrs)
			}
		case kindInt64UpDownCounter:
			var i otelmetric.Int64UpDownCounter
			if i, err = meter.Int64UpDownCounter(name); err == nil {
				i.Add(ctx, 1, attrs)
			}
		case kindFloat64UpDownCounter:
			var i otelmetric.Float64UpDownCounter
			if i, err = meter.Float64UpDownCounter(name); err == nil {
				i.Add(ctx, 1, attrs)
			}
		case kindInt64Histogram:
			var i otelmetric.Int64Histogram
			if i, err = meter.Int64Histogram(name); err == nil {
				i.Record(ctx, 1, attrs)
			}
		case kindFloat64Histogram:
			var i otelmetric.Float64Histogram
			if i, err = meter.Float64Histogram(name); err == nil {
				i.Record(ctx, 1, attrs)
			}
		case kindInt64Gauge:
			var i otelmetric.Int64Gauge
			if i, err = meter.Int64Gauge(name); err == nil {
				i.Record(ctx, 1, attrs)
			}
		case kindFloat64Gauge:
			var i otelmetric.Float64Gauge
			if i, err = meter.Float64Gauge(name); err == nil {
				i.Record(ctx, 1, attrs)
			}
		}
		if err != nil {
			t.Fatalf("creating instrument %s: %v", name, err)
		}
	}
	if err := mp.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
}

// collectExported returns every time series the fake API received, with its
// metric descriptor, in arrival order.
func collectExported(t *testing.T, fake *fakeMetricService, instruments map[string]instrumentKind) []exportedMetric {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var out []exportedMetric
	for _, ts := range fake.series {
		typ := ts.GetMetric().GetType()
		d, ok := fake.descriptors[typ]
		if !ok {
			t.Fatalf("time series %s exported without a metric descriptor", typ)
		}
		var otelName string
		for name := range instruments {
			if typ == "workload.googleapis.com/"+name {
				otelName = name
			}
		}
		if otelName == "" {
			t.Fatalf("exported metric type %q does not match any hub instrument", typ)
		}
		out = append(out, exportedMetric{
			otelName:     otelName,
			kind:         d.GetMetricKind(),
			valueType:    d.GetValueType(),
			labels:       ts.GetMetric().GetLabels(),
			resourceType: ts.GetResource().GetType(),
			resource:     ts.GetResource().GetLabels(),
		})
	}
	return out
}

// exportThroughFakeAPI exports every hub instrument for one replica and
// returns what arrived keyed by Cloud Monitoring metric type.
func exportThroughFakeAPI(t *testing.T, hubID, instanceID string) map[string]exportedMetric {
	t.Helper()
	instruments := hubRecorderInstruments(t)
	fake, addr := startFakeMonitoringAPI(t)
	exportReplica(t, addr, hubID, instruments, WithInstanceID(instanceID))
	out := map[string]exportedMetric{}
	for _, m := range collectExported(t, fake, instruments) {
		out["workload.googleapis.com/"+m.otelName] = m
	}
	if len(out) != len(instruments) {
		t.Fatalf("exported %d metric types, want one per instrument (%d)", len(out), len(instruments))
	}
	return out
}

// TestHubMetricsCloudMonitoringMapping pins how hub metrics appear in Cloud
// Monitoring (ptone/scion#3598): metric type prefix, kinds, the replica and
// deployment labels, and the monitored resource.
func TestHubMetricsCloudMonitoringMapping(t *testing.T) {
	hermeticMetricsEnv(t)
	exported := exportThroughFakeAPI(t, "hub-a", "replica-1")
	for typ, m := range exported {
		if want := "workload.googleapis.com/" + m.otelName; typ != want {
			t.Errorf("metric type = %q, want %q", typ, want)
		}
		if got := m.labels[InstanceIDLabel]; got != "replica-1" {
			t.Errorf("%s: label %s = %q, want %q (labels %v)", typ, InstanceIDLabel, got, "replica-1", m.labels)
		}
		if got := m.labels[HubIDLabel]; got != "hub-a" {
			t.Errorf("%s: label %s = %q, want %q (labels %v)", typ, HubIDLabel, got, "hub-a", m.labels)
		}
		if got := m.labels["scion_hub_name"]; got != "dashboard test hub" {
			t.Errorf("%s: label scion_hub_name = %q, want %q", typ, got, "dashboard test hub")
		}
		if m.labels["dashboard_test"] != "1" {
			t.Errorf("%s: point attribute not exported as label: %v", typ, m.labels)
		}
		if m.resourceType != "generic_task" || m.resource["task_id"] != "replica-1" {
			t.Errorf("%s: monitored resource = %s %v, want generic_task with task_id replica-1", typ, m.resourceType, m.resource)
		}
	}
	// Spot-check the kind mapping for each instrument type the hub uses.
	for name, want := range map[string]metricpb.MetricDescriptor_MetricKind{
		dispatchmetrics.MetricDispatchClaimed:       metricpb.MetricDescriptor_CUMULATIVE,
		dbmetrics.MetricPoolConnectionsActive:       metricpb.MetricDescriptor_GAUGE,
		dispatchmetrics.MetricDispatchLatency:       metricpb.MetricDescriptor_CUMULATIVE,
		reapermetrics.MetricLaunchReaperDisarmedFor: metricpb.MetricDescriptor_GAUGE,
	} {
		m := exported["workload.googleapis.com/"+name]
		if m.kind != want {
			t.Errorf("%s: kind = %v, want %v", name, m.kind, want)
		}
	}
	if vt := exported["workload.googleapis.com/"+dispatchmetrics.MetricDispatchLatency].valueType; vt != metricpb.MetricDescriptor_DISTRIBUTION {
		t.Errorf("histogram value type = %v, want DISTRIBUTION", vt)
	}
}

// TestHubReplicasWriteDistinctSeries covers the HA case: every replica shares
// one hub ID (server.hub.hub_id), so only the per-process instance ID can
// keep their series apart. Two replicas exporting the same metrics to the same
// project must differ in the service_instance_id label and in the monitored
// resource, and agree on scion_hub_id.
func TestHubReplicasWriteDistinctSeries(t *testing.T) {
	hermeticMetricsEnv(t)
	instruments := hubRecorderInstruments(t)
	fake, addr := startFakeMonitoringAPI(t)
	exportReplica(t, addr, "shared-hub", instruments, WithInstanceID("replica-1"))
	exportReplica(t, addr, "shared-hub", instruments, WithInstanceID("replica-2"))

	type seriesKey struct{ metric, instance, resource string }
	seen := map[seriesKey]bool{}
	perMetric := map[string]map[string]bool{}
	for _, m := range collectExported(t, fake, instruments) {
		if got := m.labels[HubIDLabel]; got != "shared-hub" {
			t.Errorf("%s: label %s = %q, want shared-hub", m.otelName, HubIDLabel, got)
		}
		inst := m.labels[InstanceIDLabel]
		if inst != "replica-1" && inst != "replica-2" {
			t.Errorf("%s: label %s = %q, want replica-1 or replica-2", m.otelName, InstanceIDLabel, inst)
		}
		if m.resource["task_id"] != inst {
			t.Errorf("%s: monitored resource task_id = %q, want %q", m.otelName, m.resource["task_id"], inst)
		}
		k := seriesKey{m.otelName, inst, m.resourceType + "/" + m.resource["task_id"]}
		if seen[k] {
			t.Errorf("%s: replica %s wrote the same series twice", m.otelName, inst)
		}
		seen[k] = true
		if perMetric[m.otelName] == nil {
			perMetric[m.otelName] = map[string]bool{}
		}
		perMetric[m.otelName][k.resource] = true
	}
	for name := range instruments {
		if n := len(perMetric[name]); n != 2 {
			t.Errorf("%s: %d distinct monitored resources across two replicas, want 2", name, n)
		}
	}
}

// TestHubInstanceIDFallback covers a caller that passes an empty instance ID
// or no WithInstanceID option: NewMeterProvider must still give each provider
// its own service.instance.id, so replicas cannot collapse onto one series.
func TestHubInstanceIDFallback(t *testing.T) {
	hermeticMetricsEnv(t)
	instruments := hubRecorderInstruments(t)
	fake, addr := startFakeMonitoringAPI(t)
	exportReplica(t, addr, "shared-hub", instruments, WithInstanceID(""))
	exportReplica(t, addr, "shared-hub", instruments)

	instances := map[string]bool{}
	for _, m := range collectExported(t, fake, instruments) {
		inst := m.labels[InstanceIDLabel]
		if inst == "" {
			t.Fatalf("%s: exported without %s (labels %v)", m.otelName, InstanceIDLabel, m.labels)
		}
		if m.resourceType != "generic_task" || m.resource["task_id"] != inst {
			t.Errorf("%s: monitored resource = %s %v, want generic_task with task_id %s", m.otelName, m.resourceType, m.resource, inst)
		}
		instances[inst] = true
	}
	if len(instances) != 2 {
		t.Errorf("two providers without an instance ID exported %d distinct instance IDs, want 2: %v", len(instances), instances)
	}
}

func TestResourceAttributeLabelFilter(t *testing.T) {
	cases := []struct {
		kv   attribute.KeyValue
		want bool
	}{
		{attribute.String(HubIDAttribute, "abc"), true},
		{attribute.String(HubNameAttribute, "prod"), true},
		{attribute.String(HubIDAttribute, ""), false},
		{attribute.String("service.name", "scion-hub"), true},
		{attribute.String("host.name", "node-1"), false},
	}
	for _, c := range cases {
		if got := ResourceAttributeLabelFilter(c.kv); got != c.want {
			t.Errorf("ResourceAttributeLabelFilter(%s=%q) = %v, want %v", c.kv.Key, c.kv.Value.AsString(), got, c.want)
		}
	}
}

// --- dashboard JSON --------------------------------------------------------

type dashboardAggregation struct {
	AlignmentPeriod    string   `json:"alignmentPeriod"`
	PerSeriesAligner   string   `json:"perSeriesAligner"`
	CrossSeriesReducer string   `json:"crossSeriesReducer"`
	GroupByFields      []string `json:"groupByFields"`
}

type dashboardDataSet struct {
	TimeSeriesQuery struct {
		TimeSeriesFilter *struct {
			Filter      string               `json:"filter"`
			Aggregation dashboardAggregation `json:"aggregation"`
		} `json:"timeSeriesFilter"`
	} `json:"timeSeriesQuery"`
	LegendTemplate string `json:"legendTemplate"`
}

type dashboardWidget struct {
	Title   string `json:"title"`
	XYChart *struct {
		DataSets []dashboardDataSet `json:"dataSets"`
	} `json:"xyChart"`
	SectionHeader json.RawMessage `json:"sectionHeader"`
}

type dashboardFile struct {
	Name             string `json:"name"`
	DisplayName      string `json:"displayName"`
	DashboardFilters []struct {
		LabelKey   string `json:"labelKey"`
		FilterType string `json:"filterType"`
	} `json:"dashboardFilters"`
	MosaicLayout struct {
		Columns int `json:"columns"`
		Tiles   []struct {
			XPos   int             `json:"xPos"`
			YPos   int             `json:"yPos"`
			Width  int             `json:"width"`
			Height int             `json:"height"`
			Widget dashboardWidget `json:"widget"`
		} `json:"tiles"`
	} `json:"mosaicLayout"`
}

var (
	metricTypeInFilter = regexp.MustCompile(`metric\.type\s*=\s*"([^"]+)"`)
	labelInExpr        = regexp.MustCompile(`metric\.labels?\.(?:"([^"]+)"|([A-Za-z0-9_]+))`)
)

// validAligner reports whether the dashboard may use aligner and reducer on a
// metric of the given Cloud Monitoring kind and value type.
func validAligner(kind metricpb.MetricDescriptor_MetricKind, vt metricpb.MetricDescriptor_ValueType, aligner, reducer string) bool {
	switch {
	case vt == metricpb.MetricDescriptor_DISTRIBUTION:
		return aligner == "ALIGN_DELTA" && strings.HasPrefix(reducer, "REDUCE_PERCENTILE_")
	case kind == metricpb.MetricDescriptor_CUMULATIVE:
		return (aligner == "ALIGN_RATE" || aligner == "ALIGN_DELTA") && reducer == "REDUCE_SUM"
	case kind == metricpb.MetricDescriptor_GAUGE:
		switch aligner {
		case "ALIGN_MEAN", "ALIGN_MAX", "ALIGN_MIN", "ALIGN_NEXT_OLDER":
			return reducer == "REDUCE_MAX" || reducer == "REDUCE_MEAN" || reducer == "REDUCE_MIN" || reducer == "REDUCE_SUM"
		}
	}
	return false
}

// TestHubDashboardJSON checks the importable hub dashboard
// (deploy/monitoring/dashboards/scion-hub.json, ptone/scion#3599): every
// chart queries a metric the hub exports, with an aligner that fits the
// metric kind, grouped by hub replica, and the file names no project.
func TestHubDashboardJSON(t *testing.T) {
	hermeticMetricsEnv(t)
	raw, err := os.ReadFile(hubDashboardPath)
	if err != nil {
		t.Fatalf("reading dashboard: %v", err)
	}
	var d dashboardFile
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parsing dashboard JSON: %v", err)
	}

	// No project may be baked in: "name" carries projects/<id>/dashboards/<id>
	// and is assigned at import time.
	if d.Name != "" {
		t.Errorf("dashboard must not set name (it embeds a project): %q", d.Name)
	}
	if strings.Contains(string(raw), "projects/") || strings.Contains(string(raw), "project_id") {
		t.Error("dashboard JSON must not reference a project")
	}
	if d.DisplayName == "" {
		t.Error("dashboard needs a displayName")
	}
	hasHubFilter := false
	for _, f := range d.DashboardFilters {
		if f.LabelKey == HubIDLabel && f.FilterType == "METRIC_LABEL" {
			hasHubFilter = true
		}
	}
	if !hasHubFilter {
		t.Errorf("dashboard should offer a %s METRIC_LABEL filter", HubIDLabel)
	}

	exported := exportThroughFakeAPI(t, "hub-a", "replica-1")
	replicaGroup := `metric.label."` + InstanceIDLabel + `"`
	charted := map[string]bool{}
	charts := 0
	if d.MosaicLayout.Columns <= 0 {
		t.Fatal("mosaicLayout.columns must be set")
	}
	for i, tile := range d.MosaicLayout.Tiles {
		w := tile.Widget
		if tile.XPos+tile.Width > d.MosaicLayout.Columns || tile.Width <= 0 || tile.Height <= 0 {
			t.Errorf("tile %d (%q) does not fit the %d-column grid", i, w.Title, d.MosaicLayout.Columns)
		}
		if w.SectionHeader != nil {
			continue
		}
		if w.XYChart == nil {
			t.Errorf("tile %d (%q): only xyChart and sectionHeader widgets are expected", i, w.Title)
			continue
		}
		charts++
		if len(w.XYChart.DataSets) == 0 {
			t.Errorf("chart %q has no data sets", w.Title)
		}
		for _, ds := range w.XYChart.DataSets {
			tsf := ds.TimeSeriesQuery.TimeSeriesFilter
			if tsf == nil {
				t.Errorf("chart %q: data set must use timeSeriesQuery.timeSeriesFilter", w.Title)
				continue
			}
			m := metricTypeInFilter.FindStringSubmatch(tsf.Filter)
			if m == nil {
				t.Errorf("chart %q: filter %q has no metric.type", w.Title, tsf.Filter)
				continue
			}
			em, ok := exported[m[1]]
			if !ok {
				t.Errorf("chart %q: metric %q is not exported by the hub recorders", w.Title, m[1])
				continue
			}
			charted[m[1]] = true

			agg := tsf.Aggregation
			if !validAligner(em.kind, em.valueType, agg.PerSeriesAligner, agg.CrossSeriesReducer) {
				t.Errorf("chart %q: aligner %s / reducer %s does not fit %s %s metric %s",
					w.Title, agg.PerSeriesAligner, agg.CrossSeriesReducer, em.kind, em.valueType, m[1])
			}
			if agg.AlignmentPeriod == "" {
				t.Errorf("chart %q: alignmentPeriod must be set", w.Title)
			}
			if len(agg.GroupByFields) == 0 || agg.GroupByFields[0] != replicaGroup {
				t.Errorf("chart %q: first groupByField must be %s, got %v", w.Title, replicaGroup, agg.GroupByFields)
			}

			// Every label referenced must reach Cloud Monitoring: a
			// resource-derived label (checked against the export above) or
			// a known per-point attribute of this metric.
			exprs := append([]string{tsf.Filter, ds.LegendTemplate}, agg.GroupByFields...)
			for _, e := range exprs {
				for _, lm := range labelInExpr.FindAllStringSubmatch(e, -1) {
					key := lm[1] + lm[2]
					if _, ok := em.labels[key]; ok && key != "dashboard_test" {
						continue
					}
					if !pointAttributeLabels[em.otelName][key] {
						t.Errorf("chart %q: label %q is not exported for %s", w.Title, key, em.otelName)
					}
				}
			}
		}
	}
	if charts == 0 {
		t.Fatal("dashboard has no charts")
	}

	// The panels the hub health design (F2) calls for must all be present.
	for _, name := range []string{
		dbmetrics.MetricPoolConnectionsActive,
		dbmetrics.MetricPoolConnectionsIdle,
		dbmetrics.MetricPoolConnectionsWaiting,
		dbmetrics.MetricPoolConnectionsMax,
		dispatchmetrics.MetricDispatchClaimed,
		dispatchmetrics.MetricDispatchDone,
		dispatchmetrics.MetricDispatchFailed,
		dispatchmetrics.MetricMessageStuck,
		dispatchmetrics.MetricDispatchLatency,
		dbmetrics.MetricPublishToDeliverLatency,
		dbmetrics.MetricNotificationsDropped,
		reapermetrics.MetricLaunchReaperTicks,
		reapermetrics.MetricLaunchReaperRowErrors,
	} {
		if !charted["workload.googleapis.com/"+name] {
			t.Errorf("dashboard is missing a chart for %s", name)
		}
	}

	var notCharted []string
	for typ := range exported {
		if !charted[typ] {
			notCharted = append(notCharted, typ)
		}
	}
	sort.Strings(notCharted)
	t.Logf("exported hub metrics not on the dashboard: %v", notCharted)
}
