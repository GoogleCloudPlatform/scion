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
	"os"
	"strings"
	"time"

	mexporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

const defaultExportInterval = 60 * time.Second

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

	exporter, err := mexporter.New(mexporter.WithProjectID(gcpProjectID))
	if err != nil {
		return nil, fmt.Errorf("creating GCP metric exporter: %w", err)
	}

	resAttrs := []attribute.KeyValue{
		semconv.ServiceName("scion-hub"),
	}
	if o.hubID != "" {
		resAttrs = append(resAttrs, attribute.String("scion.hub.id", o.hubID))
	}
	if envHubID := os.Getenv("SCION_HUB_ID"); envHubID != "" && o.hubID == "" {
		resAttrs = append(resAttrs, attribute.String("scion.hub.id", envHubID))
	}
	if o.hubName != "" {
		resAttrs = append(resAttrs, attribute.String("scion.hub.name", o.hubName))
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(resAttrs...),
	)
	if err != nil {
		return nil, fmt.Errorf("creating OTel resource: %w", err)
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
