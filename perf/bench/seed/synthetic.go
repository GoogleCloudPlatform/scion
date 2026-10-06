package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// phaseWeights approximates a "mostly-settled, some churn" large project:
// heavy on running/stopped/error (per ptone/scion#2367's evidence, these
// dominate real projects), with a smaller tail of early-lifecycle and
// suspended agents. Values are relative weights, not percentages.
var phaseWeights = []struct {
	phase  string
	weight int
}{
	{"running", 45},
	{"stopped", 20},
	{"error", 15},
	{"created", 5},
	{"provisioning", 3},
	{"cloning", 2},
	{"starting", 3},
	{"suspended", 5},
	{"stopping", 2},
}

// activitiesForRunning mirrors pkg/agent/state.Activity's meaningful values
// for a "running" agent (Activity is only meaningful when Phase == running).
var activitiesForRunning = []string{
	"working", "thinking", "executing", "waiting_for_input", "blocked",
}

func pickWeightedPhase(rng *rand.Rand) string {
	total := 0
	for _, w := range phaseWeights {
		total += w.weight
	}
	r := rng.Intn(total)
	for _, w := range phaseWeights {
		if r < w.weight {
			return w.phase
		}
		r -= w.weight
	}
	return phaseWeights[len(phaseWeights)-1].phase
}

// syntheticAgent builds one realistic Agent row: a status drawn from
// phaseWeights, an appliedConfig sized per the 90/10 small/large split
// observed to matter in ptone/scion#2367 (a small minority of agents with a
// large inlined pre-start hook script account for a disproportionate share
// of response bytes), and an ancestry chain drawn from none / single-level /
// multi-level per ptone/scion#2393's "mix of ... ancestry" requirement.
//
// Returns the agent plus labels describing which ancestry/size bucket it
// landed in, so the caller can tally reproducible distribution counts for
// the seed metadata file.
func syntheticAgent(rng *rand.Rand, projectID, ownerID, memberID string, index int, priorAgentIDs []string) (agent *store.Agent, ancestryKind, sizeKind string) {
	now := time.Now()
	phase := pickWeightedPhase(rng)
	activity := ""
	if phase == "running" {
		activity = activitiesForRunning[rng.Intn(len(activitiesForRunning))]
	}

	id := uuid.NewString()
	a := &store.Agent{
		ID:        id,
		Slug:      fmt.Sprintf("bench-agent-%05d", index),
		Name:      fmt.Sprintf("Bench Agent %d", index),
		Template:  "default",
		ProjectID: projectID,
		Phase:     phase,
		Activity:  activity,
		Labels: map[string]string{
			"bench":      "true",
			"bench-seq":  fmt.Sprintf("%d", index),
			"bench-tier": sizeTierLabel(index),
		},
		CreatedBy: pickCreator(rng, ownerID, memberID),
		OwnerID:   pickCreator(rng, ownerID, memberID),
		Created:   now.Add(-time.Duration(rng.Intn(30*24)) * time.Hour),
		Updated:   now.Add(-time.Duration(rng.Intn(24)) * time.Hour),
	}
	if phase != "created" && phase != "provisioning" {
		a.LastSeen = a.Updated
	}

	a.Ancestry, ancestryKind = syntheticAncestry(rng, ownerID, memberID, priorAgentIDs)
	a.AppliedConfig, sizeKind = syntheticAppliedConfig(rng, index)

	return a, ancestryKind, sizeKind
}

func pickCreator(rng *rand.Rand, ownerID, memberID string) string {
	if rng.Intn(2) == 0 {
		return ownerID
	}
	return memberID
}

// syntheticAncestry distributes agents across three ancestry shapes:
// 60% no ancestry (created directly, no transitive lineage to track),
// 25% single-level (created by a user principal directly),
// 15% multi-level (created by an agent that was itself created by a user --
// exercises store.AgentFilter.AncestorID / progeny-grant evaluation the same
// way a real sub-agent spawn chain would).
func syntheticAncestry(rng *rand.Rand, ownerID, memberID string, priorAgentIDs []string) ([]string, string) {
	root := ownerID
	if rng.Intn(2) == 0 {
		root = memberID
	}
	switch r := rng.Intn(100); {
	case r < 60:
		return nil, "none"
	case r < 85 || len(priorAgentIDs) == 0:
		return []string{root}, "single"
	default:
		parent := priorAgentIDs[rng.Intn(len(priorAgentIDs))]
		return []string{root, parent}, "chain"
	}
}

func sizeTierLabel(index int) string {
	if index%10 == 0 {
		return "large"
	}
	return "small"
}

// syntheticAppliedConfig produces a small (~90% of agents, a few hundred
// bytes of JSON) or large (~10%, several KB via an inlined pre-start hook
// script) appliedConfig payload. ptone/scion#2367 measured a captured
// project-agent response spending ~62% of its bytes on appliedConfig; this
// split exists to reproduce that skew rather than a uniform small payload
// that would understate response-size cost.
func syntheticAppliedConfig(rng *rand.Rand, index int) (*store.AgentAppliedConfig, string) {
	ac := &store.AgentAppliedConfig{
		Image:         "ghcr.io/scion-project/claude-harness:latest",
		HarnessConfig: "default-claude",
		Model:         "claude-sonnet-5",
		Profile:       "default",
		TemplateID:    "tmpl-bench-default",
		TemplateHash:  "sha256:" + strings.Repeat("ab", 32),
		AgentRole:     "baseline",
		CreatorName:   "bench-owner@example.test",
		Env: map[string]string{
			"SCION_PROJECT":   "bench-project",
			"SCION_AGENT_SEQ": fmt.Sprintf("%d", index),
			"SCION_BENCH_RUN": "1",
			"SCION_LOG_LEVEL": "info",
		},
	}

	if index%10 != 0 {
		return ac, "small"
	}

	// ~10% large: an inlined pre-start hook script padded to a few KB, the
	// single field the investigation called out as bounded to 64 KB and the
	// largest realistic contributor to appliedConfig size.
	ac.ProjectPreStartHookID = uuid.NewString()
	ac.ProjectPreStartHookScript = syntheticHookScript(rng)
	return ac, "large"
}

// syntheticHookScript returns a plausible multi-KB shell script: repeated,
// individually-varied lines rather than one repeated string, so gzip/JSON
// size estimates aren't artificially compressible in a way a real script
// wouldn't be.
func syntheticHookScript(rng *rand.Rand) string {
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\nset -euo pipefail\n")
	lines := 80 + rng.Intn(40) // ~80-120 lines, several KB total
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "echo 'bench pre-start step %d: %d'\n", i, rng.Int63())
	}
	return b.String()
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
