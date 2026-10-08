/*
Copyright 2026 The Scion Authors.
*/

// Package hubmetrics creates the OpenTelemetry MeterProvider used by hub-side
// metric recorders (dbmetrics, dispatchmetrics). It exports directly to GCP
// Cloud Monitoring via Application Default Credentials.
package hubmetrics

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	mexporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

const defaultExportInterval = 60 * time.Second

// Resource attributes and Cloud Monitoring labels that identify where a hub
// metric came from. The exporter turns a resource attribute into a metric
// label by replacing each character that is not a letter or digit with "_".
const (
	// HubIDAttribute identifies the hub deployment. Every replica of an HA
	// hub shares one hub ID (server.hub.hub_id), so it does not tell
	// replicas apart. In Cloud Monitoring it becomes the label HubIDLabel.
	HubIDAttribute = "scion.hub.id"
	// HubNameAttribute carries the hub's display name. In Cloud Monitoring it
	// becomes the label "scion_hub_name". It is not a reliable replica
	// identity; do not group by it.
	HubNameAttribute = "scion.hub.name"
	// HubIDLabel is the Cloud Monitoring metric label for HubIDAttribute.
	// Dashboards filter by it to select one hub deployment.
	HubIDLabel = "scion_hub_id"
	// InstanceIDLabel is the Cloud Monitoring metric label for the
	// service.instance.id resource attribute, set from the hub's
	// per-process instance ID (WithInstanceID). It is distinct for each
	// replica and changes on every restart. Dashboards group by it to show
	// each replica separately.
	InstanceIDLabel = "service_instance_id"
)

// MetricGroup identifies a logical group of hub metrics that can be
// independently enabled or disabled.
type MetricGroup struct {
	EnvVar      string
	NamePattern string
}

var metricGroups = []MetricGroup{
	{EnvVar: "SCION_METRICS_DB_NOTIFY", NamePattern: "scion.db.notify.*"},
	{EnvVar: "SCION_METRICS_DB_POOL", NamePattern: "scion.db.pool.*"},
	{EnvVar: "SCION_METRICS_DISPATCH", NamePattern: "scion.dispatch.*"},
	{EnvVar: "SCION_METRICS_HUB_AUTH", NamePattern: "scion.hub.auth.*"},
	{EnvVar: "SCION_METRICS_HUB_AUTH", NamePattern: "scion.hub.registration.*"},
	{EnvVar: "SCION_METRICS_HUB_AUTH", NamePattern: "scion.hub.join.*"},
	{EnvVar: "SCION_METRICS_HUB_AUTH", NamePattern: "scion.hub.rotation.*"},
	{EnvVar: "SCION_METRICS_HUB_AUTH", NamePattern: "scion.hub.brokers.*"},
	{EnvVar: "SCION_METRICS_HUB_AUTH", NamePattern: "scion.hub.dispatch.*"},
	{EnvVar: "SCION_METRICS_HUB_GCP", NamePattern: "scion.hub.gcp.*"},
}

// Option configures the MeterProvider.
type Option func(*options)

type options struct {
	exportInterval time.Duration
	hubID          string
	hubName        string
	instanceID     string
	// exporterOpts are extra Cloud Monitoring exporter options. Tests use
	// them to point the exporter at an in-process fake API server.
	exporterOpts []mexporter.Option
}

// WithExportInterval sets the periodic reader interval. Defaults to 60s.
func WithExportInterval(d time.Duration) Option {
	return func(o *options) { o.exportInterval = d }
}

// WithHubID sets the scion.hub.id resource attribute.
func WithHubID(id string) Option {
	return func(o *options) { o.hubID = id }
}

// WithHubName sets the scion.hub.name resource attribute.
func WithHubName(name string) Option {
	return func(o *options) { o.hubName = name }
}

// WithInstanceID sets the service.instance.id resource attribute to the hub
// replica's per-process instance ID. The exporter writes it as the metric
// label InstanceIDLabel and maps the resource to a generic_task monitored
// resource whose task_id is this ID, so each replica writes its own series.
// If the ID is empty (or the option is omitted), NewMeterProvider generates a
// random one, so replicas never share a series.
func WithInstanceID(id string) Option {
	return func(o *options) { o.instanceID = id }
}

// withExporterOptions appends Cloud Monitoring exporter options. Test only.
func withExporterOptions(opts ...mexporter.Option) Option {
	return func(o *options) { o.exporterOpts = append(o.exporterOpts, opts...) }
}

// NewMeterProvider creates an OTel SDK MeterProvider that exports to GCP Cloud
// Monitoring. It uses Application Default Credentials (workload identity on
// Cloud Run, attached SA on GCE).
//
// If gcpProjectID is empty, an error is returned — callers should fall back to
// disabled recorders.
func NewMeterProvider(ctx context.Context, gcpProjectID string, opts ...Option) (*metric.MeterProvider, error) {
	if gcpProjectID == "" {
		return nil, fmt.Errorf("GCP project ID is required for hub metrics export")
	}

	o := &options{exportInterval: defaultExportInterval}
	for _, fn := range opts {
		fn(o)
	}

	exporterOpts := append([]mexporter.Option{
		mexporter.WithProjectID(gcpProjectID),
		mexporter.WithFilteredResourceAttributes(ResourceAttributeLabelFilter),
	}, o.exporterOpts...)
	baseExporter, err := mexporter.New(exporterOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating GCP metric exporter: %w", err)
	}
	exporter := &loggingExporter{delegate: baseExporter}

	res, err := newResource(ctx, o)
	if err != nil {
		return nil, err
	}

	mpOpts := []metric.Option{
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(exporter,
			metric.WithInterval(o.exportInterval),
		)),
	}

	mpOpts = append(mpOpts, groupDropViews()...)

	return metric.NewMeterProvider(mpOpts...), nil
}

// NewResource builds the OTel resource NewMeterProvider attaches to every
// hub metric from opts, without creating an exporter. Callers use it to check
// the identity a set of options produces.
func NewResource(ctx context.Context, opts ...Option) (*resource.Resource, error) {
	o := &options{exportInterval: defaultExportInterval}
	for _, fn := range opts {
		fn(o)
	}
	return newResource(ctx, o)
}

// newResource builds the OTel resource attached to every hub metric.
func newResource(ctx context.Context, o *options) (*resource.Resource, error) {
	resAttrs := []attribute.KeyValue{
		semconv.ServiceName("scion-hub"),
	}
	if o.hubID != "" {
		resAttrs = append(resAttrs, attribute.String(HubIDAttribute, o.hubID))
	}
	if envHubID := os.Getenv("SCION_HUB_ID"); envHubID != "" && o.hubID == "" {
		resAttrs = append(resAttrs, attribute.String(HubIDAttribute, envHubID))
	}
	if o.hubName != "" {
		resAttrs = append(resAttrs, attribute.String(HubNameAttribute, o.hubName))
	}
	instanceID := o.instanceID
	if instanceID == "" {
		// Without service.instance.id every replica of a hub would write the
		// same series, so never export without one.
		instanceID = uuid.NewString()
		slog.Warn("hub metrics: no hub instance ID supplied; using a generated one",
			"service_instance_id", instanceID)
	}
	resAttrs = append(resAttrs, semconv.ServiceInstanceID(instanceID))

	res, err := resource.New(ctx,
		resource.WithAttributes(resAttrs...),
	)
	if err != nil {
		return nil, fmt.Errorf("creating OTel resource: %w", err)
	}
	return res, nil
}

// ResourceAttributeLabelFilter selects the resource attributes the Cloud
// Monitoring exporter copies onto every exported point as metric labels. By
// default the exporter copies only service.name, service.namespace and
// service.instance.id and drops every other resource attribute, so without
// this filter scion.hub.id and scion.hub.name would never reach Cloud
// Monitoring. This filter keeps the default set (service.instance.id carries
// the per-replica identity) and adds the hub deployment attributes, so
// several hubs exporting to one project can be told apart.
func ResourceAttributeLabelFilter(kv attribute.KeyValue) bool {
	if mexporter.DefaultResourceAttributesFilter(kv) {
		return true
	}
	switch string(kv.Key) {
	case HubIDAttribute, HubNameAttribute:
		return kv.Value.AsString() != ""
	}
	return false
}

// groupDropViews returns OTel View options that drop instruments belonging to
// disabled metric groups. A group is disabled when its env var is set to
// "false" or "0". All groups are enabled by default.
func groupDropViews() []metric.Option {
	var opts []metric.Option
	for _, g := range metricGroups {
		if isGroupDisabled(g.EnvVar) {
			opts = append(opts, metric.WithView(metric.NewView(
				metric.Instrument{Name: g.NamePattern},
				metric.Stream{Aggregation: metric.AggregationDrop{}},
			)))
		}
	}
	return opts
}

func isGroupDisabled(envVar string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(envVar)))
	return v == "false" || v == "0"
}

// GroupScopes returns the instrumentation scopes for each metric group, useful
// for testing and documentation.
func GroupScopes() []MetricGroup {
	return append([]MetricGroup(nil), metricGroups...)
}

// InstrumentationScope returns a scope matching the dbmetrics or
// dispatchmetrics package, useful for building Views in tests.
func InstrumentationScope(name string) instrumentation.Scope {
	return instrumentation.Scope{Name: name}
}

// loggingExporter wraps a metric.Exporter and logs any export errors with
// structured context before returning them. This gives hub operators
// visibility into metric export failures that the OTel SDK would otherwise
// only surface through the global error handler.
type loggingExporter struct {
	delegate metric.Exporter
}

var _ metric.Exporter = (*loggingExporter)(nil)

func (e *loggingExporter) Temporality(k metric.InstrumentKind) metricdata.Temporality {
	return e.delegate.Temporality(k)
}

func (e *loggingExporter) Aggregation(k metric.InstrumentKind) metric.Aggregation {
	return e.delegate.Aggregation(k)
}

func (e *loggingExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	err := e.delegate.Export(ctx, rm)
	if err != nil {
		metricCount := 0
		if rm != nil {
			for _, sm := range rm.ScopeMetrics {
				metricCount += len(sm.Metrics)
			}
		}
		slog.Error("hub metrics export error",
			"error", err,
			"metric_count", metricCount,
		)
	}
	return err
}

func (e *loggingExporter) ForceFlush(ctx context.Context) error {
	return e.delegate.ForceFlush(ctx)
}

func (e *loggingExporter) Shutdown(ctx context.Context) error {
	return e.delegate.Shutdown(ctx)
}
