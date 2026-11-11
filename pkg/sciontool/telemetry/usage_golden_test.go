/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"encoding/json"
	"flag"
	"os"
	"sort"
	"testing"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// usageGoldenPath is the checked-in emitter output pkg/hub's dashboard golden
// test loads by relative path (design §7.6): hub must not import
// sciontool, so a plain file — not a Go type — is the cross-package
// contract. Keep the two in sync: pkg/hub/metrics_dashboard_golden_test.go
// reads "../sciontool/telemetry/" + usageGoldenPath.
const usageGoldenPath = "testdata/usage/claude-2.1.280.timeseries.json"

// updateUsageGolden refreshes usageGoldenPath from whatever
// TestPipelineDerivesClaudeUsageEndToEnd actually captures, instead of
// comparing against it: `go test ./pkg/sciontool/telemetry/... -run
// TestPipelineDerivesClaudeUsageEndToEnd -update`.
var updateUsageGolden = flag.Bool("update", false, "update golden test fixtures")

// usageGoldenFixture is the on-disk shape of usageGoldenPath: one entry in
// Flushes per GCP export call captured, each a protojson-marshaled
// monitoringpb.TimeSeries. Two flushes of the same identities (§7.6) is what
// lets this fixture pin the cumulative, epoch-stable shape — not just a
// single snapshot's label set.
type usageGoldenFixture struct {
	Source           string              `json:"source"`
	ClaudeCLIVersion string              `json:"claude_cli_version"`
	Flushes          [][]json.RawMessage `json:"flushes"`
}

// checkOrUpdateUsageGolden is the golden check: with -update it writes
// flushes (one []*TimeSeries per captured GCP export call) to
// usageGoldenPath; otherwise it loads that file and requires an exact
// (proto.Equal) match, series for series, flush for flush.
func checkOrUpdateUsageGolden(t *testing.T, flushes [][]*monitoringpb.TimeSeries) {
	t.Helper()
	marshaler := protojson.MarshalOptions{Indent: "  "}
	got := usageGoldenFixture{
		Source:           "TestPipelineDerivesClaudeUsageEndToEnd (pkg/sciontool/telemetry)",
		ClaudeCLIVersion: "2.1.280",
	}
	for _, flush := range flushes {
		raws := make([]json.RawMessage, len(flush))
		for i, ts := range flush {
			b, err := marshaler.Marshal(ts)
			if err != nil {
				t.Fatalf("marshaling captured series %d for the golden fixture: %v", i, err)
			}
			raws[i] = json.RawMessage(b)
		}
		got.Flushes = append(got.Flushes, raws)
	}

	if *updateUsageGolden {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatalf("marshaling golden fixture: %v", err)
		}
		if err := os.WriteFile(usageGoldenPath, append(b, '\n'), 0o644); err != nil {
			t.Fatalf("writing golden fixture %s: %v", usageGoldenPath, err)
		}
		t.Logf("updated golden fixture %s", usageGoldenPath)
		return
	}

	data, err := os.ReadFile(usageGoldenPath)
	if err != nil {
		t.Fatalf("reading golden fixture %s (rerun with -update to create it): %v", usageGoldenPath, err)
	}
	var want usageGoldenFixture
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatalf("unmarshaling golden fixture %s: %v", usageGoldenPath, err)
	}
	if len(want.Flushes) != len(got.Flushes) {
		t.Fatalf("golden fixture %s has %d flushes, captured %d (rerun with -update to refresh)", usageGoldenPath, len(want.Flushes), len(got.Flushes))
	}
	for i := range want.Flushes {
		wantSeries := unmarshalGoldenSeries(t, want.Flushes[i])
		gotSeries := unmarshalGoldenSeries(t, got.Flushes[i])
		if len(wantSeries) != len(gotSeries) {
			t.Fatalf("flush %d: golden has %d series, captured %d (rerun with -update to refresh)", i, len(wantSeries), len(gotSeries))
		}
		// snapshotGCP iterates a Go map, so the order of series within one
		// flush is not stable across runs even though the set of series is.
		// Sort both sides on a canonical (metric type + labels) key before
		// comparing series pairwise.
		sortSeriesCanonically(wantSeries)
		sortSeriesCanonically(gotSeries)
		for j := range wantSeries {
			if !proto.Equal(wantSeries[j], gotSeries[j]) {
				t.Errorf("flush %d series %d differs from golden fixture %s (rerun with -update to refresh):\n golden=%v\ncaptured=%v",
					i, j, usageGoldenPath, wantSeries[j], gotSeries[j])
			}
		}
	}
}

// seriesSortKey is a canonical, order-independent identity for a TimeSeries:
// its metric type plus its labels sorted by key. Two series with the same
// identity (as GCP defines it) always produce the same key regardless of
// map-iteration order upstream.
func seriesSortKey(ts *monitoringpb.TimeSeries) string {
	keys := make([]string, 0, len(ts.GetMetric().GetLabels()))
	for k := range ts.GetMetric().GetLabels() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	key := ts.GetMetric().GetType()
	for _, k := range keys {
		key += "\x00" + k + "=" + ts.GetMetric().GetLabels()[k]
	}
	return key
}

func sortSeriesCanonically(series []*monitoringpb.TimeSeries) {
	sort.Slice(series, func(i, j int) bool { return seriesSortKey(series[i]) < seriesSortKey(series[j]) })
}

func unmarshalGoldenSeries(t *testing.T, raws []json.RawMessage) []*monitoringpb.TimeSeries {
	t.Helper()
	out := make([]*monitoringpb.TimeSeries, len(raws))
	for i, raw := range raws {
		var ts monitoringpb.TimeSeries
		if err := protojson.Unmarshal(raw, &ts); err != nil {
			t.Fatalf("unmarshaling golden series %d: %v", i, err)
		}
		out[i] = &ts
	}
	return out
}
