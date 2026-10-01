// Command apibench drives repeated timed requests against a hub started
// against a perf/bench/seed-produced database, and reports median/spread API
// latency separated from the browser/rendering measurements that
// web/e2e-perf/large-project-bench.mjs covers.
//
// It is the second stage of the perf/2393-large-project-bench harness
// (ptone/scion#2393, #2374, #2367).
//
// Usage:
//
//	go run ./perf/bench/apibench \
//	  --hub http://127.0.0.1:19810 \
//	  --seed /tmp/scion-bench/seed-25.json \
//	  --runs 5 \
//	  --out /tmp/scion-bench/api-25.json
//
// Two scenarios are always run against the seed's project:
//
//   - "project-agents-list": GET /api/v1/projects/{id}/agents -- what the
//     project grid/list page loads (ptone/scion#2367's dominant measured
//     bottleneck).
//   - "global-agents-list": GET /api/v1/agents?projectId={id} -- what the
//     standalone/global agent graph page loads (web/src/components/pages/
//     agent-graph.ts fetches this same unscoped endpoint and filters
//     client-side; passing projectId here reproduces the
//     "standalone project-filtered graph" row from the original
//     investigation's evidence table).
//
// Every request authenticates as the seed's non-admin project-member
// principal, per the brief: the default-deny/per-resource evaluation path
// for an ordinary member is where authorization cost shows up.
//
// A per-attempt failure (client timeout, connection error, or a non-2xx
// status) is recorded, not fatal: the run continues and the report is still
// written, with success/failure counts and median/min/max/stddev computed
// over successful attempts only (bench-rev-1 B3/N1/N2). At agent counts
// near or above the hub's default 60s WriteTimeout
// (pkg/config/hub_config.go's HubServerConfig.WriteTimeout /
// pkg/hub/web.go's WebServer.Start), some attempts failing is itself part
// of the measurement, not noise to discard.
//
// When the hub under test was built from the perf/2392-agent-list-
// instrumentation branch (not yet merged; see that branch's pkg/hub/
// perftrace*.go) AND started with SCION_HUB_PERF_TRACE=1, passing
// --want-perf-trace additionally sets the opt-in X-Scion-Perf-Trace request
// header and records whatever trace fields come back in the response
// headers. On a plain baseline run against unmodified main there is no such
// data, and the report's perfTraceAvailable field is false rather than
// silently omitted (only true when at least one attempt actually returned
// trace headers).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/perf/bench/internal/benchout"
)

const perfTraceRequestHeader = "X-Scion-Perf-Trace"

func main() {
	hubURL := flag.String("hub", "http://127.0.0.1:19810", "hub base URL")
	seedPath := flag.String("seed", "", "path to seed metadata JSON produced by perf/bench/seed (required)")
	runs := flag.Int("runs", 5, "number of timed trials per scenario (median/spread reported over these)")
	warmup := flag.Int("warmup", 1, "number of untimed warmup requests per scenario before the timed runs (failures here are also recorded, not fatal)")
	outPath := flag.String("out", "", "path to write the JSON report (required)")
	wantPerfTrace := flag.Bool("want-perf-trace", false, "set the opt-in X-Scion-Perf-Trace header and record any perf-trace response data (only meaningful on a #2392-instrumented hub build)")
	notes := flag.String("notes", "", "free-form note about machine/CPU conditions for this run, copied into the report")
	timeoutSeconds := flag.Int("timeout-seconds", 60, "per-request client timeout; the unmodified-main baseline at 500 agents can exceed the 60s default, per ptone/scion#2367's superlinear-scaling diagnosis")
	flag.Parse()

	if *seedPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "usage: apibench --seed <seed.json> --out <report.json> [--hub url] [--runs N] [--warmup N] [--want-perf-trace] [--notes text]")
		os.Exit(2)
	}

	seedData, err := os.ReadFile(*seedPath)
	if err != nil {
		log.Fatalf("read seed metadata: %v", err)
	}
	var seed benchout.SeedMetadata
	if err := json.Unmarshal(seedData, &seed); err != nil {
		log.Fatalf("parse seed metadata: %v", err)
	}

	client := &http.Client{
		Timeout: time.Duration(*timeoutSeconds) * time.Second,
	}

	scenarios := []struct {
		name     string
		endpoint string
	}{
		{"project-agents-list", fmt.Sprintf("/api/v1/projects/%s/agents", seed.ProjectID)},
		{"global-agents-list", fmt.Sprintf("/api/v1/agents?projectId=%s", seed.ProjectID)},
	}

	report := benchout.APIBenchReport{
		GeneratedAt: time.Now().UTC(),
		HubBaseURL:  *hubURL,
		// bench-rev-1 N6: the seed metadata embeds long-lived bearer tokens
		// and the session secret. redactSeed strips them before anything
		// gets written to disk, so a report file can never leak a working
		// credential even if an uploader forgets to redact by hand.
		Seed: redactSeed(seed),
		Machine: benchout.MachineInfo{
			GOOS:      runtime.GOOS,
			GOARCH:    runtime.GOARCH,
			NumCPU:    runtime.NumCPU(),
			GoVersion: runtime.Version(),
			Notes:     *notes,
		},
	}
	// bench-rev-1 N8: record load/uptime automatically, since budgets
	// cannot be chosen from a shared, variably-loaded host -- see
	// perf/bench/README.md's "Choosing regression budgets" section for what
	// this is and is not sufficient for.
	report.Machine.LoadAvg1, report.Machine.LoadAvg5, report.Machine.LoadAvg15 = readLoadAvg()
	report.Machine.UptimeSeconds = readUptimeSeconds()
	if h, err := os.Hostname(); err == nil {
		report.Machine.Hostname = h
	}

	exitCode := 0
	for _, sc := range scenarios {
		stats := runScenario(client, *hubURL, sc.name, sc.endpoint, seed.MemberToken, seed.AgentCount, *runs, *warmup, *wantPerfTrace)
		report.Scenarios = append(report.Scenarios, stats)
		fmt.Printf("%-24s agents=%-5d success=%d/%d median_total=%.1fms median_ttfb=%.1fms min=%.1fms max=%.1fms stddev=%.1fms\n",
			sc.name, seed.AgentCount, stats.SuccessCount, stats.SuccessCount+stats.FailureCount,
			stats.MedianTotalMs, stats.MedianTTFBMs, stats.MinTotalMs, stats.MaxTotalMs, stats.StdDevTotalMs)
		if stats.FailureCount > 0 {
			for _, a := range stats.Attempts {
				if !a.Success {
					fmt.Printf("  FAILED attempt %d: status=%d error=%q\n", a.Index, a.Status, a.Error)
				}
			}
			// A failure is a real measurement (e.g. the WriteTimeout cliff
			// documented in perf/bench/README.md), not a tool bug -- still
			// write the report and exit non-zero only to flag it for CI/log
			// scanning, never by discarding the data (bench-rev-1 N1).
			exitCode = 1
		}
	}

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		log.Fatalf("marshal report: %v", err)
	}
	if err := os.WriteFile(*outPath, data, 0o600); err != nil {
		log.Fatalf("write report: %v", err)
	}
	fmt.Printf("report written to %s\n", *outPath)
	os.Exit(exitCode)
}

// redactSeed returns a copy of seed with long-lived credentials removed.
// See benchout.SeedMetadata's field docs for which fields these are.
func redactSeed(seed benchout.SeedMetadata) benchout.SeedMetadata {
	redacted := seed
	redacted.SessionSecret = "REDACTED"
	redacted.OwnerToken = "REDACTED"
	redacted.MemberToken = "REDACTED"
	return redacted
}

func runScenario(client *http.Client, hubURL, name, endpoint, token string, agentCount, runs, warmup int, wantPerfTrace bool) benchout.ScenarioStats {
	stats := benchout.ScenarioStats{
		Name:       name,
		Endpoint:   endpoint,
		AgentCount: agentCount,
		Runs:       runs,
	}

	doOne := func(index int) benchout.Attempt {
		a := benchout.Attempt{Index: index}

		req, err := http.NewRequest(http.MethodGet, hubURL+endpoint, nil)
		if err != nil {
			a.Error = err.Error()
			return a
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if wantPerfTrace {
			req.Header.Set(perfTraceRequestHeader, "1")
		}

		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			// bench-rev-1 N1: a timeout or connection error is a result,
			// not a tool crash -- record it and let the caller continue.
			a.Error = err.Error()
			a.TotalMs = msf(time.Since(start))
			return a
		}
		defer func() { _ = resp.Body.Close() }()
		a.Status = resp.StatusCode
		a.TTFBMs = msf(time.Since(start)) // approximate: headers-received time via client.Do return

		n, err := io.Copy(io.Discard, resp.Body)
		a.TotalMs = msf(time.Since(start))
		if err != nil {
			a.Error = fmt.Sprintf("reading body: %v", err)
			return a
		}
		a.Bytes = n

		// bench-rev-1 N2: a non-2xx status is a failed attempt, not a timed
		// success -- the caller must not average it in with real successes.
		a.Success = a.Status >= 200 && a.Status < 300
		if !a.Success {
			a.Error = fmt.Sprintf("non-2xx status %d", a.Status)
		}

		if wantPerfTrace {
			trace := map[string]string{}
			for _, k := range []string{"X-Scion-Perf-Phases", "X-Scion-Perf-Store-Calls", "X-Scion-Perf-Decisions"} {
				if v := resp.Header.Get(k); v != "" {
					trace[k] = v
				}
			}
			// bench-rev-1 N3: only report trace data as available when it
			// actually came back, not merely because --want-perf-trace was
			// passed (e.g. hitting an unmodified-main hub with the flag on).
			if len(trace) > 0 {
				a.PerfTrace = trace
			}
		}
		return a
	}

	index := 0
	for i := 0; i < warmup; i++ {
		doOne(index) // warmup failures are not fatal and are not recorded
		index++
	}

	for i := 0; i < runs; i++ {
		a := doOne(index)
		index++
		stats.Attempts = append(stats.Attempts, a)
		if a.Success {
			stats.SuccessCount++
		} else {
			stats.FailureCount++
		}
		if a.PerfTrace != nil {
			stats.PerfTraceAvailable = true
		}
	}

	var successTTFB, successTotal []float64
	for _, a := range stats.Attempts {
		if !a.Success {
			continue
		}
		successTTFB = append(successTTFB, a.TTFBMs)
		successTotal = append(successTotal, a.TotalMs)
	}
	stats.MedianTTFBMs = median(successTTFB)
	stats.MedianTotalMs = median(successTotal)
	stats.MinTotalMs, stats.MaxTotalMs = minMax(successTotal)
	stats.StdDevTotalMs = stddev(successTotal)
	return stats
}

func msf(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[mid-1] + sorted[mid]) / 2
	}
	return sorted[mid]
}

func minMax(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	lo, hi := xs[0], xs[0]
	for _, x := range xs[1:] {
		if x < lo {
			lo = x
		}
		if x > hi {
			hi = x
		}
	}
	return lo, hi
}

func stddev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var mean float64
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	var sumSq float64
	for _, x := range xs {
		d := x - mean
		sumSq += d * d
	}
	return math.Sqrt(sumSq / float64(len(xs)-1))
}
