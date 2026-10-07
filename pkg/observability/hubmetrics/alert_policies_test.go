/*
Copyright 2026 The Scion Authors.
*/

package hubmetrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"gopkg.in/yaml.v3"
)

// alertPoliciesPath holds the Cloud Monitoring alert policy specifications.
var alertPoliciesPath = filepath.Join("..", "..", "..", "deploy", "monitoring", "alert-policies.yaml")

// nonHubRecorderMetrics are alert policy metrics exported through the same
// Google Cloud metric exporter (so also as workload.googleapis.com/<name>)
// but registered outside the hub recorders that hubRecorderInstruments
// captures: sciontool's telemetry pipeline self metrics
// (pkg/sciontool/telemetry/pipeline.go).
var nonHubRecorderMetrics = map[string]exportedMetric{
	"scion.telemetry.pipeline.status": {kind: metricpb.MetricDescriptor_GAUGE, valueType: metricpb.MetricDescriptor_INT64},
	"scion.telemetry.export.errors":   {kind: metricpb.MetricDescriptor_CUMULATIVE, valueType: metricpb.MetricDescriptor_INT64},
}

// pendingAlertMetrics are metrics an alert policy names before any code
// registers them. Each entry needs a matching prerequisite note in the
// policy file. An entry must be removed once the metric is exported.
var pendingAlertMetrics = map[string]bool{
	"scion.hub.auth.broker.verify.failures": true,
}

type alertPolicySpec struct {
	DisplayName       string `yaml:"displayName"`
	Metric            string `yaml:"metric"`
	DenominatorMetric string `yaml:"denominatorMetric"`
	ConditionType     string `yaml:"conditionType"`
	Aggregation       struct {
		PerSeriesAligner string `yaml:"perSeriesAligner"`
	} `yaml:"aggregation"`
}

// validAlertAligner reports whether an alert condition may align a metric of
// the given kind and value type with aligner.
func validAlertAligner(kind metricpb.MetricDescriptor_MetricKind, vt metricpb.MetricDescriptor_ValueType, aligner string) bool {
	switch {
	case vt == metricpb.MetricDescriptor_DISTRIBUTION:
		return strings.HasPrefix(aligner, "ALIGN_PERCENTILE_") || aligner == "ALIGN_DELTA"
	case kind == metricpb.MetricDescriptor_CUMULATIVE:
		return aligner == "ALIGN_DELTA" || aligner == "ALIGN_RATE"
	case kind == metricpb.MetricDescriptor_GAUGE:
		switch aligner {
		case "ALIGN_MEAN", "ALIGN_MAX", "ALIGN_MIN", "ALIGN_NEXT_OLDER":
			return true
		}
	}
	return false
}

// TestAlertPolicyMetricTypes cross-checks deploy/monitoring/alert-policies.yaml
// against what reaches Cloud Monitoring (ptone/scion#3616): every policy
// metric type must be workload.googleapis.com/<a metric the hub exports>,
// aligned in a way that fits its kind.
func TestAlertPolicyMetricTypes(t *testing.T) {
	hermeticMetricsEnv(t)
	raw, err := os.ReadFile(alertPoliciesPath)
	if err != nil {
		t.Fatalf("reading alert policies: %v", err)
	}
	var file struct {
		AlertPolicies []alertPolicySpec `yaml:"alertPolicies"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parsing alert policies: %v", err)
	}
	if len(file.AlertPolicies) == 0 {
		t.Fatal("no alert policies found")
	}

	const prefix = "workload.googleapis.com/"
	exported := exportThroughFakeAPI(t, "hub-a", "replica-1")
	for name := range pendingAlertMetrics {
		if _, ok := exported[prefix+name]; ok {
			t.Errorf("%s is now exported by the hub; remove it from pendingAlertMetrics", name)
		}
	}

	for _, p := range file.AlertPolicies {
		for _, typ := range []string{p.Metric, p.DenominatorMetric} {
			if typ == "" {
				continue
			}
			name, ok := strings.CutPrefix(typ, prefix)
			if !ok {
				t.Errorf("policy %q: metric type %q must start with %s", p.DisplayName, typ, prefix)
				continue
			}
			if pendingAlertMetrics[name] {
				continue
			}
			em, ok := exported[typ]
			if !ok {
				em, ok = nonHubRecorderMetrics[name]
			}
			if !ok {
				t.Errorf("policy %q: metric %q is not exported", p.DisplayName, typ)
				continue
			}
			if p.ConditionType == "ABSENCE" {
				continue
			}
			if !validAlertAligner(em.kind, em.valueType, p.Aggregation.PerSeriesAligner) {
				t.Errorf("policy %q: aligner %s does not fit %s %s metric %s",
					p.DisplayName, p.Aggregation.PerSeriesAligner, em.kind, em.valueType, typ)
			}
		}
	}
}
