package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/perf/bench/internal/benchout"
)

// The hub sends X-Scion-Perf-* headers only to unscoped local platform
// admins; the bench drives traffic as a non-admin member. The hub still
// logs one perf_trace line per request (server.hub.perf_trace), keyed by
// request ID, so mergeHubPerfLog joins those lines to attempts and stores
// them in Attempt.PerfTrace in the same header-shaped format, with
// PerfTraceSource "hub-log". The log line also carries the serialize phase,
// which headers cannot.

// perfLogLine is the subset of a perf_trace JSON log line used here.
type perfLogLine map[string]any

func (l perfLogLine) int(key string) (int64, bool) {
	v, ok := l[key].(float64)
	return int64(v), ok
}

// perfTraceFromLogLine renders a perf_trace line as header-shaped values.
func perfTraceFromLogLine(l perfLogLine) map[string]string {
	phasesUs, phasesN := map[string]int64{}, map[string]int64{}
	storeN, storeUs := map[string]int64{}, map[string]int64{}
	for k := range l {
		v, ok := l.int(k)
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(k, "phase_") && strings.HasSuffix(k, "_us"):
			phasesUs[strings.TrimSuffix(strings.TrimPrefix(k, "phase_"), "_us")] = v
		case strings.HasPrefix(k, "phase_") && strings.HasSuffix(k, "_n"):
			phasesN[strings.TrimSuffix(strings.TrimPrefix(k, "phase_"), "_n")] = v
		case strings.HasPrefix(k, "store_") && strings.HasSuffix(k, "_n"):
			storeN[strings.TrimSuffix(strings.TrimPrefix(k, "store_"), "_n")] = v
		case strings.HasPrefix(k, "store_") && strings.HasSuffix(k, "_us"):
			storeUs[strings.TrimSuffix(strings.TrimPrefix(k, "store_"), "_us")] = v
		}
	}
	join := func(m map[string]int64) string {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+strconv.FormatInt(m[k], 10))
		}
		return strings.Join(parts, ",")
	}
	get := func(k string) int64 { v, _ := l.int(k); return v }
	out := map[string]string{}
	if e, ok := l["endpoint"].(string); ok {
		out["X-Scion-Perf-Endpoint"] = e
	}
	for name, v := range map[string]string{
		"X-Scion-Perf-Phases":       join(phasesUs),
		"X-Scion-Perf-Phase-Counts": join(phasesN),
		"X-Scion-Perf-Store-Calls":  join(storeN),
		"X-Scion-Perf-Store-Us":     join(storeUs),
	} {
		if v != "" {
			out[name] = v
		}
	}
	out["X-Scion-Perf-Decisions"] = fmt.Sprintf("count=%d,allow=%d,deny=%d,other=%d,audit_us=%d",
		get("audit_records"), get("audit_allow"), get("audit_deny"), get("audit_other"), get("audit_emit_us"))
	if _, ok := l["db_open"]; ok {
		out["X-Scion-Perf-DB"] = fmt.Sprintf("wait_count=%d,wait_us=%d,in_use=%d,open=%d",
			get("db_wait_count"), get("db_wait_us"), get("db_in_use"), get("db_open"))
	}
	return out
}

// mergeHubPerfLog reads the hub's JSON log at path and fills PerfTrace on
// every attempt whose request ID matches a perf_trace line and that has no
// header-sourced trace. It returns the number of attempts joined.
func mergeHubPerfLog(report *benchout.APIBenchReport, path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	byID := map[string]perfLogLine{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), `"perf_trace"`) {
			continue
		}
		var l perfLogLine
		if json.Unmarshal(line, &l) != nil || l["msg"] != "perf_trace" {
			continue
		}
		if id, ok := l["request_id"].(string); ok && id != "" {
			byID[id] = l
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	joined := 0
	for i := range report.Scenarios {
		s := &report.Scenarios[i]
		for j := range s.Attempts {
			a := &s.Attempts[j]
			if a.PerfTrace != nil || a.RequestID == "" {
				continue
			}
			if l, ok := byID[a.RequestID]; ok {
				a.PerfTrace = perfTraceFromLogLine(l)
				a.PerfTraceSource = "hub-log"
				joined++
			}
		}
		for _, a := range s.Attempts {
			if len(a.PerfTrace) > 0 {
				s.PerfTraceAvailable = true
			}
		}
	}
	return joined, nil
}
