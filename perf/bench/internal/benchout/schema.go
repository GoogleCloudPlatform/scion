// Package benchout defines the small JSON schemas shared between the
// perf/bench tools (seed, apibench, and the browser benchmark's Go-side
// glue, if any). Keeping these in one place means the seed tool and the
// benchmark tools that consume its output cannot drift out of sync silently.
package benchout

import "time"

// SeedMetadata is written by perf/bench/seed after it finishes populating a
// hub SQLite database with one project and N synthetic agents. Every other
// bench tool reads this file to find the project, credentials, and expected
// counts rather than re-deriving them.
type SeedMetadata struct {
	// DBPath is the sqlite file the hub subprocess must be started against
	// (via `scion server start --db <DBPath> --session-secret <SessionSecret>`).
	DBPath string `json:"dbPath"`
	// SessionSecret must be passed to the hub subprocess unchanged: the
	// member/owner bearer tokens below are signed with a key derived from it,
	// and only a hub started with the same secret will derive the same key.
	SessionSecret string `json:"sessionSecret"`

	ProjectID   string `json:"projectId"`
	ProjectSlug string `json:"projectSlug"`

	OwnerUserID string `json:"ownerUserId"`
	OwnerEmail  string `json:"ownerEmail"`
	OwnerToken  string `json:"ownerToken"`

	// MemberUserID/MemberToken belong to a project-member (not project-owner,
	// not hub-admin) principal. This is the requester the benchmarks drive
	// traffic as: per ptone/scion#2367, the default-deny/per-resource
	// evaluation path for an ordinary member is where authorization cost
	// shows up, not the admin/owner fast paths.
	MemberUserID string `json:"memberUserId"`
	MemberEmail  string `json:"memberEmail"`
	MemberToken  string `json:"memberToken"`

	AgentCount int `json:"agentCount"`
	// PhaseCounts/ActivityCounts/AncestryCounts record the actual synthetic
	// distribution produced (seeding is randomized but seeded, so these are
	// reproducible for a given --rand-seed, but are still recorded rather
	// than assumed).
	PhaseCounts             map[string]int `json:"phaseCounts"`
	ActivityCounts          map[string]int `json:"activityCounts"`
	AncestryCounts          map[string]int `json:"ancestryCounts"`          // "none" | "single" | "chain"
	AppliedConfigSizeCounts map[string]int `json:"appliedConfigSizeCounts"` // "small" | "large"

	RandSeed int64     `json:"randSeed"`
	SeededAt time.Time `json:"seededAt"`
}

// APIBenchReport is written by perf/bench/apibench after running its timed
// trials against a seeded project.
type APIBenchReport struct {
	GeneratedAt time.Time `json:"generatedAt"`
	// HarnessCommit is the harness's own build-time VCS revision.
	// It comes from `runtime/debug.ReadBuildInfo()`'s `vcs.revision` build
	// setting -- i.e. the commit the *running binary* was built from -- not
	// from running `git rev-parse HEAD` in the process's current working
	// directory, which silently records whatever checkout the operator
	// happens to invoke the binary from (possibly a different one than it
	// was built in) and is wrong whenever those differ. Empty whenever Go's
	// VCS stamping could not supply a revision; see HarnessCommitSource for
	// why.
	HarnessCommit string `json:"harnessCommit,omitempty"`
	// HarnessCommitDirty is nil when HarnessCommit itself is unknown (no
	// VCS stamp at all -- see HarnessCommitSource), and otherwise points to
	// whether the build-time working tree had uncommitted changes
	// (`vcs.modified`). A bare `omitempty bool` would make "the tree was
	// clean" and "dirty state is unknown" both serialize as a missing field
	// -- indistinguishable to a reader. A pointer makes "known clean"
	// (`false`), "known dirty" (`true`), and "unknown" (absent) three
	// different, correctly distinguishable states.
	HarnessCommitDirty *bool `json:"harnessCommitDirty,omitempty"`
	// HarnessCommitSource explains how HarnessCommit was obtained, or why
	// it is empty -- recorded explicitly rather than left for the reader to
	// guess. Normally "go build VCS stamp"; see perf/bench/README.md for
	// why `-buildvcs=false` must NOT be passed, and why the tools must be
	// built from a regular clone rather than any form of `git worktree`
	// checkout. A worktree OUTSIDE any other checkout gets no stamp at all
	// (source is "unavailable: ..."); a worktree NESTED inside another
	// checkout -- including this repo's own gitignored
	// `.claude/worktrees/<name>` -- is silently stamped with the ENCLOSING
	// checkout's commit instead and can report clean, which this field
	// alone cannot distinguish from a correct stamp. See the README for the
	// independent `go version -m` verification step.
	HarnessCommitSource string `json:"harnessCommitSource"`
	// HubScionVersion identifies the hub binary under test, read from its
	// own unauthenticated GET /health (pkg/hub/handlers_health.go's
	// `scionVersion` field, `pkg/version.Short()`) -- covering build
	// provenance for the hub side as well as the harness's own. Empty if
	// /health could not be reached or parsed, and "unknown" if the hub
	// binary itself was not built with version info or a VCS stamp (see
	// perf/bench/README.md).
	//
	// There is deliberately no HubVersion field. Recording /health's
	// `version` field under that name would look like real provenance but
	// is actually pkg/hub's hard-coded `"0.1.0"` placeholder
	// (`handlers_health.go:86`, marked `// TODO: Get from build info`) --
	// constant regardless of which hub commit is actually running. A field
	// that always reads the same value no matter what is measured is worse
	// than no field, the same reasoning applied to the harness's own commit
	// field above.
	HubScionVersion   string            `json:"hubScionVersion,omitempty"`
	HubBaseURL        string            `json:"hubBaseUrl"`
	EffectiveSettings EffectiveSettings `json:"effectiveSettings"`
	Seed              SeedMetadata      `json:"seed"`
	Scenarios         []ScenarioStats   `json:"scenarios"`
	Machine           MachineInfo       `json:"machine"`
}

// EffectiveSettings records the resolved CLI flags a report was produced
// with, so a reader does not have to guess defaults.
type EffectiveSettings struct {
	Runs           int  `json:"runs"`
	Warmup         int  `json:"warmup"`
	TimeoutSeconds int  `json:"timeoutSeconds"`
	WantPerfTrace  bool `json:"wantPerfTrace"`
}

// Attempt records one HTTP request attempt, success or failure. A failure
// -- a client timeout, a connection error, or a non-2xx status -- is a
// measurement result, not a tool error: it is recorded here and excluded
// from ScenarioStats' median/min/max/stddev, never silently dropped or
// averaged in as if it were a timed success.
type Attempt struct {
	Index int `json:"index"`
	// Status is 0 for a request that never got a response at all (client
	// timeout or connection error) -- see Error for why.
	Status int `json:"status"`
	// Success is true only for a 2xx status with a fully-read body.
	Success bool `json:"success"`
	// Error is non-empty whenever Success is false: the client error text,
	// a body-read error, or "non-2xx status NNN".
	Error   string  `json:"error,omitempty"`
	Bytes   int64   `json:"bytes,omitempty"`
	TTFBMs  float64 `json:"ttfbMs,omitempty"`
	TotalMs float64 `json:"totalMs"`
	// PerfTrace is set only when the hub was started with request
	// performance tracing on (server.hub.perf_trace), the request carried
	// the opt-in X-Scion-Perf-Trace header, AND the response actually
	// included trace headers -- not merely because --want-perf-trace was
	// passed. Keys are the X-Scion-Perf-* header names.
	PerfTrace map[string]string `json:"perfTrace,omitempty"`
}

// ScenarioStats holds repeated-trial timing for one (endpoint, agentCount)
// combination, e.g. "project-agents-list" at 100 agents.
type ScenarioStats struct {
	Name       string    `json:"name"`
	Endpoint   string    `json:"endpoint"`
	AgentCount int       `json:"agentCount"`
	Runs       int       `json:"runs"` // requested timed-run count
	Attempts   []Attempt `json:"attempts"`

	SuccessCount int `json:"successCount"`
	FailureCount int `json:"failureCount"`

	// Median/Min/Max/StdDev are computed over successful attempts only: a
	// run that timed out or errored contributes to SuccessCount/
	// FailureCount and appears in Attempts, but never pulls these numbers
	// toward itself the way including a ~300s timeout duration in a median
	// would.
	MedianTTFBMs  float64 `json:"medianTtfbMs"`
	MedianTotalMs float64 `json:"medianTotalMs"`
	MinTotalMs    float64 `json:"minTotalMs"`
	MaxTotalMs    float64 `json:"maxTotalMs"`
	StdDevTotalMs float64 `json:"stddevTotalMs"`

	// PerfTraceAvailable is true only if at least one Attempt's PerfTrace is
	// non-empty -- see Attempt.PerfTrace.
	PerfTraceAvailable bool `json:"perfTraceAvailable"`
}

// MachineInfo records enough about the run environment to caveat comparisons
// across runs/machines, per the brief's "note machine and CPU conditions".
//
// LoadAvg*/UptimeSeconds are read automatically from /proc (Linux only, zero
// elsewhere): budgets cannot be chosen from this host -- a shared,
// variably-loaded container -- and a future reader comparing runs needs
// the load figure alongside the latency numbers to tell environment noise
// from a real regression.
type MachineInfo struct {
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
	NumCPU    int    `json:"numCpu"`
	GoVersion string `json:"goVersion"`
	Hostname  string `json:"hostname"`
	Notes     string `json:"notes"`

	LoadAvg1      float64 `json:"loadAvg1,omitempty"`
	LoadAvg5      float64 `json:"loadAvg5,omitempty"`
	LoadAvg15     float64 `json:"loadAvg15,omitempty"`
	UptimeSeconds float64 `json:"uptimeSeconds,omitempty"`

	// *AtEnd are sampled again after the run completes: a 500-agent run can
	// take many minutes, long enough for load to swing sharply within a
	// single report. A report whose start/end load differ a lot flags
	// itself as having run through a noise spike rather than steady-state
	// conditions.
	LoadAvg1AtEnd  float64 `json:"loadAvg1AtEnd,omitempty"`
	LoadAvg5AtEnd  float64 `json:"loadAvg5AtEnd,omitempty"`
	LoadAvg15AtEnd float64 `json:"loadAvg15AtEnd,omitempty"`
}
