/*
Copyright 2026 The Scion Authors.
*/

package hubmetrics

import (
	"context"
	"os"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/observability/dbmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/dispatchmetrics"
)

func TestNewMeterProviderEmptyProjectID(t *testing.T) {
	_, err := NewMeterProvider(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty project ID")
	}
}

func TestGroupDropViewsAllEnabled(t *testing.T) {
	for _, g := range metricGroups {
		if err := os.Unsetenv(g.EnvVar); err != nil {
			t.Fatalf("Unsetenv(%s): %v", g.EnvVar, err)
		}
	}
	views := groupDropViews()
	if len(views) != 0 {
		t.Errorf("expected 0 drop views when all groups enabled, got %d", len(views))
	}
}

func TestGroupDropViewsDisabled(t *testing.T) {
	t.Setenv("SCION_METRICS_DB_NOTIFY", "false")

	views := groupDropViews()
	if len(views) != 1 {
		t.Errorf("expected 1 drop view, got %d", len(views))
	}
}

func TestGroupDropViewsDisabledZero(t *testing.T) {
	t.Setenv("SCION_METRICS_DISPATCH", "0")

	views := groupDropViews()
	if len(views) != 1 {
		t.Errorf("expected 1 drop view, got %d", len(views))
	}
}

func TestIsGroupDisabled(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"", false},
		{"true", false},
		{"1", false},
		{"false", true},
		{"0", true},
	}

	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			envVar := "SCION_METRICS_TEST_GROUP"
			if tc.value != "" {
				t.Setenv(envVar, tc.value)
			} else {
				if err := os.Unsetenv(envVar); err != nil {
					t.Fatalf("Unsetenv(%s): %v", envVar, err)
				}
			}
			if got := isGroupDisabled(envVar); got != tc.want {
				t.Errorf("isGroupDisabled(%q=%q) = %v, want %v", envVar, tc.value, got, tc.want)
			}
		})
	}
}

func TestRecordersEnabledWithRealProvider(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	dbRec, err := dbmetrics.New(mp)
	if err != nil {
		t.Fatalf("dbmetrics.New: %v", err)
	}
	if !dbRec.Enabled() {
		t.Error("dbmetrics.Recorder should be enabled with real MeterProvider")
	}

	dispRec, err := dispatchmetrics.New(mp)
	if err != nil {
		t.Fatalf("dispatchmetrics.New: %v", err)
	}
	if !dispRec.Enabled() {
		t.Error("dispatchmetrics.Recorder should be enabled with real MeterProvider")
	}
}

func TestDropViewPreventsExport(t *testing.T) {
	t.Setenv("SCION_METRICS_DB_NOTIFY", "false")

	reader := metric.NewManualReader()
	mpOpts := []metric.Option{metric.WithReader(reader)}
	mpOpts = append(mpOpts, groupDropViews()...)
	mp := metric.NewMeterProvider(mpOpts...)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	dbRec, err := dbmetrics.New(mp)
	if err != nil {
		t.Fatalf("dbmetrics.New: %v", err)
	}

	ctx := context.Background()
	dbRec.IncPublished(ctx, 1)
	dbRec.IncDelivered(ctx, 1)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == dbmetrics.MetricNotificationsPublished ||
				m.Name == dbmetrics.MetricNotificationsDelivered {
				t.Errorf("metric %q should have been dropped by view, but was exported", m.Name)
			}
		}
	}
}

func TestPoolMetricsNotDroppedWhenNotifyDisabled(t *testing.T) {
	t.Setenv("SCION_METRICS_DB_NOTIFY", "false")

	reader := metric.NewManualReader()
	mpOpts := []metric.Option{metric.WithReader(reader)}
	mpOpts = append(mpOpts, groupDropViews()...)
	mp := metric.NewMeterProvider(mpOpts...)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	dbRec, err := dbmetrics.New(mp)
	if err != nil {
		t.Fatalf("dbmetrics.New: %v", err)
	}

	ctx := context.Background()
	dbRec.ObservePoolStats(ctx, dbmetrics.PoolStats{Active: 5, Idle: 3, Waiting: 0, Max: 10})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}

	names := make(map[string]bool)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
		}
	}

	if !names[dbmetrics.MetricPoolConnectionsActive] {
		t.Error("pool metric should still be exported when only db-notify is disabled")
	}
}
