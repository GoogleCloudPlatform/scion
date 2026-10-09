/*
Copyright 2026 The Scion Authors.
*/

package hubmetrics

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
	DisplayName       string  `yaml:"displayName"`
	Metric            string  `yaml:"metric"`
	DenominatorMetric string  `yaml:"denominatorMetric"`
	ConditionType     string  `yaml:"conditionType"`
	Threshold         float64 `yaml:"threshold"`
	// ThresholdComment is the threshold line's trailing comment, filled in
	// from the YAML node tree.
	ThresholdComment string `yaml:"-"`
	Aggregation      struct {
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
	policies := parseAlertPolicies(t, raw)

	const prefix = "workload.googleapis.com/"
	exported := exportThroughFakeAPI(t, "hub-a", "replica-1")
	for name := range pendingAlertMetrics {
		if _, ok := exported[prefix+name]; ok {
			t.Errorf("%s is now exported by the hub; remove it from pendingAlertMetrics", name)
		}
	}

	for _, p := range policies {
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

// parseAlertPolicies decodes the alert policies and records each threshold
// line's trailing comment.
func parseAlertPolicies(t *testing.T, raw []byte) []alertPolicySpec {
	t.Helper()
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatalf("parsing alert policies: %v", err)
	}
	var file struct {
		AlertPolicies []yaml.Node `yaml:"alertPolicies"`
	}
	if err := root.Decode(&file); err != nil {
		t.Fatalf("decoding alert policies: %v", err)
	}
	if len(file.AlertPolicies) == 0 {
		t.Fatal("no alert policies found")
	}
	out := make([]alertPolicySpec, 0, len(file.AlertPolicies))
	for i := range file.AlertPolicies {
		n := &file.AlertPolicies[i]
		var p alertPolicySpec
		if err := n.Decode(&p); err != nil {
			t.Fatalf("decoding alert policy %d: %v", i, err)
		}
		for j := 0; j+1 < len(n.Content); j += 2 {
			if n.Content[j].Value == "threshold" {
				p.ThresholdComment = n.Content[j+1].LineComment
			}
		}
		out = append(out, p)
	}
	return out
}

// secondsComment matches a threshold comment such as "# 30 seconds, in ms".
var secondsComment = regexp.MustCompile(`^#\s*(\d+(?:\.\d+)?) seconds\b`)

// TestAlertPolicyLatencyThresholdUnits pins latency thresholds to the unit
// the hub records them in (ptone/scion#3616): every threshold on a
// millisecond histogram is at least one second, and a threshold annotated
// "# N seconds" equals N*1000. A threshold written in seconds against an ms
// histogram would page on every normal request.
func TestAlertPolicyLatencyThresholdUnits(t *testing.T) {
	raw, err := os.ReadFile(alertPoliciesPath)
	if err != nil {
		t.Fatalf("reading alert policies: %v", err)
	}
	units := hubHistogramUnits(t)
	checked := 0
	for _, p := range parseAlertPolicies(t, raw) {
		name, _ := strings.CutPrefix(p.Metric, "workload.googleapis.com/")
		if units[name] != "ms" {
			continue
		}
		checked++
		if p.Threshold < 1000 {
			t.Errorf("policy %q: threshold %v on ms histogram %s is under one second; thresholds must be in ms",
				p.DisplayName, p.Threshold, p.Metric)
		}
		if m := secondsComment.FindStringSubmatch(p.ThresholdComment); m != nil {
			secs, _ := strconv.ParseFloat(m[1], 64)
			if p.Threshold != secs*1000 {
				t.Errorf("policy %q: threshold %v does not match its comment %q", p.DisplayName, p.Threshold, p.ThresholdComment)
			}
		} else {
			t.Errorf("policy %q: ms threshold needs a \"# N seconds, in ms\" comment, got %q", p.DisplayName, p.ThresholdComment)
		}
	}
	if checked == 0 {
		t.Fatal("no latency policies checked; the ms histogram lookup is broken")
	}
}
