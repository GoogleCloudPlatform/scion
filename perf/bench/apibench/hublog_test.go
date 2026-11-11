package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/perf/bench/internal/benchout"
)

func TestMergeHubPerfLogJoinsByRequestID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.log")
	log := `{"time":"t","level":"INFO","msg":"Request completed","request_id":"r1"}
not json at all
{"time":"t","level":"INFO","msg":"perf_trace","endpoint":"agents.project.legacy","elapsed_us":900,"phase_capabilities_us":500,"phase_capabilities_n":1,"phase_serialize_us":7,"phase_serialize_n":1,"store_GetUser_n":84,"store_GetUser_us":400,"store_GetEffectiveGroups_n":1,"store_GetEffectiveGroups_us":3,"authz_store_calls":85,"authz_store_us":403,"audit_records":255,"audit_allow":162,"audit_deny":93,"audit_other":0,"audit_emit_us":186,"db_wait_count":3,"db_wait_us":40,"db_in_use":1,"db_open":1,"method":"GET","request_id":"r1"}
{"time":"t","level":"INFO","msg":"perf_trace","endpoint":"other","elapsed_us":5,"method":"GET","request_id":"r9"}
`
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	report := &benchout.APIBenchReport{Scenarios: []benchout.ScenarioStats{{
		Name: "project-agents-list",
		Attempts: []benchout.Attempt{
			{Index: 0, RequestID: "r1", Success: true},
			{Index: 1, RequestID: "missing", Success: true},
			{Index: 2, Success: true},
			{Index: 3, RequestID: "r9", PerfTrace: map[string]string{"X-Scion-Perf-Endpoint": "kept"}, PerfTraceSource: "headers"},
		},
	}}}
	n, err := mergeHubPerfLog(report, path)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("joined %d, want 1", n)
	}
	s := report.Scenarios[0]
	if !s.PerfTraceAvailable {
		t.Error("PerfTraceAvailable should be true")
	}
	got := s.Attempts[0]
	if got.PerfTraceSource != "hub-log" {
		t.Errorf("source = %q", got.PerfTraceSource)
	}
	want := map[string]string{
		"X-Scion-Perf-Endpoint":     "agents.project.legacy",
		"X-Scion-Perf-Phases":       "capabilities=500,serialize=7",
		"X-Scion-Perf-Phase-Counts": "capabilities=1,serialize=1",
		"X-Scion-Perf-Store-Calls":  "GetEffectiveGroups=1,GetUser=84",
		"X-Scion-Perf-Store-Us":     "GetEffectiveGroups=3,GetUser=400",
		"X-Scion-Perf-Decisions":    "count=255,allow=162,deny=93,other=0,audit_us=186",
		"X-Scion-Perf-DB":           "wait_count=3,wait_us=40,in_use=1,open=1",
	}
	if len(got.PerfTrace) != len(want) {
		t.Errorf("got %d keys, want %d: %v", len(got.PerfTrace), len(want), got.PerfTrace)
	}
	for k, v := range want {
		if got.PerfTrace[k] != v {
			t.Errorf("%s = %q, want %q", k, got.PerfTrace[k], v)
		}
	}
	if s.Attempts[1].PerfTrace != nil || s.Attempts[2].PerfTrace != nil {
		t.Error("unmatched attempts must stay empty")
	}
	if s.Attempts[3].PerfTrace["X-Scion-Perf-Endpoint"] != "kept" || s.Attempts[3].PerfTraceSource != "headers" {
		t.Error("header-sourced trace must not be overwritten")
	}
}

func TestMergeHubPerfLogMissingFile(t *testing.T) {
	if _, err := mergeHubPerfLog(&benchout.APIBenchReport{}, filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error")
	}
}
