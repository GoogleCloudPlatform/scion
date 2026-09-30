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
	GeneratedAt time.Time       `json:"generatedAt"`
	HubBaseURL  string          `json:"hubBaseUrl"`
	Seed        SeedMetadata    `json:"seed"`
	Scenarios   []ScenarioStats `json:"scenarios"`
	Machine     MachineInfo     `json:"machine"`
}

// ScenarioStats holds repeated-trial timing for one (endpoint, agentCount)
// combination, e.g. "project-agents-list" at 100 agents.
type ScenarioStats struct {
	Name          string    `json:"name"`
	Endpoint      string    `json:"endpoint"`
	AgentCount    int       `json:"agentCount"`
	Runs          int       `json:"runs"`
	StatusCodes   []int     `json:"statusCodes"`
	ResponseBytes []int64   `json:"responseBytes"`
	TTFBMs        []float64 `json:"ttfbMs"`
	TotalMs       []float64 `json:"totalMs"`
	MedianTTFBMs  float64   `json:"medianTtfbMs"`
	MedianTotalMs float64   `json:"medianTotalMs"`
	MinTotalMs    float64   `json:"minTotalMs"`
	MaxTotalMs    float64   `json:"maxTotalMs"`
	StdDevTotalMs float64   `json:"stddevTotalMs"`
	// PerfTrace* fields are populated only when the hub was started with the
	// #2392 instrumentation build and SCION_HUB_PERF_TRACE=1, and the request
	// carried the opt-in X-Scion-Perf-Trace header. Absent (nil) on a plain
	// baseline run against unmodified main -- see the README's "Baseline vs
	// instrumented" section.
	PerfTraceAvailable bool                `json:"perfTraceAvailable"`
	PerfTraceSamples   []map[string]string `json:"perfTraceSamples,omitempty"`
}

// MachineInfo records enough about the run environment to caveat comparisons
// across runs/machines, per the brief's "note machine and CPU conditions".
type MachineInfo struct {
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
	NumCPU    int    `json:"numCpu"`
	GoVersion string `json:"goVersion"`
	Hostname  string `json:"hostname"`
	Notes     string `json:"notes"`
}
