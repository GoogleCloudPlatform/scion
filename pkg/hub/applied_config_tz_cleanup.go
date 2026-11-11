// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hub

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// AppliedConfigTZCleanupExecutor runs adoptLegacyTZ over every agent row in
// one pass. It is the eager form of the legacy TZ classification that every
// TZ reader and writer already performs lazily (see adoptLegacyTZ): an agent
// written before ExplicitTimezone existed has its persisted TZ moved into
// ExplicitTimezone with ExplicitTimezoneLegacy set, so it reports timezone
// source "legacy", and both env copies are stripped. It is an optional
// maintenance migration (see resolveMaintenanceExecutor, key
// "applied-config-tz-cleanup"); nothing runs it automatically. On its own,
// skipping it changes nothing, because the lazy adoption still runs on each
// read; it matters only relative to applied-config-env-cleanup (see below).
// It also gives a single count of the agents whose saved TZ became a pin,
// which makes the adopted pins findable.
//
// Order relative to applied-config-env-cleanup. Both orders are safe:
//
//   - This cleanup first: TZ leaves both env maps, so the env cleanup finds
//     no TZ to judge and never touches ExplicitTimezone.
//   - The env cleanup first: it strips an AppliedConfig.Env TZ that has no
//     live plain source (a TZ injected from a runtime profile or the hub
//     default is not one), so this cleanup has nothing to adopt for that
//     agent and the agent follows the TZ resolver from then on. A TZ that
//     matches InlineConfig.Env (a configure-page pin) is kept by the env
//     cleanup, and the env cleanup removes an InlineConfig.Env key only
//     when it names a live secret, so this cleanup still adopts that value.
//
// Agent values are never logged, only agent IDs and counts. Every write goes
// through the same optimistic-lock retry pattern as the env cleanup, so the
// operation is safe to run alongside normal traffic, and it is idempotent: a
// second run finds nothing to adopt.
type AppliedConfigTZCleanupExecutor struct {
	Store store.Store
}

// appliedConfigTZCleanupResult summarizes one run.
type appliedConfigTZCleanupResult struct {
	AgentsScanned int `json:"agentsScanned"`
	// AgentsAdopted counts agents whose saved TZ became a legacy pin.
	AgentsAdopted int `json:"agentsAdopted"`
	// AgentsStripped counts agents whose env TZ was removed without a new
	// pin: an empty TZ marker, an existing pin, or an explicit unpin.
	AgentsStripped int `json:"agentsStripped"`
}

// tzAdoptOutcome is what adoptLegacyTZ did to one agent config.
type tzAdoptOutcome int

const (
	tzAdoptNone tzAdoptOutcome = iota
	tzAdoptStripped
	tzAdoptAdopted
)

// adoptLegacyTZOutcome runs adoptLegacyTZ on ac and classifies the change.
// It adds no rule of its own: an agent counts as adopted exactly when
// adoptLegacyTZ set a pin that was not there before.
func adoptLegacyTZOutcome(ac *store.AgentAppliedConfig) tzAdoptOutcome {
	if ac == nil {
		return tzAdoptNone
	}
	hadPin := ac.ExplicitTimezone != ""
	if !adoptLegacyTZ(ac) {
		return tzAdoptNone
	}
	if !hadPin && ac.ExplicitTimezone != "" {
		return tzAdoptAdopted
	}
	return tzAdoptStripped
}

func (e *AppliedConfigTZCleanupExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	_, err := e.run(ctx, logger, params)
	return err
}

// run does the work of Run and returns the summary it logs.
func (e *AppliedConfigTZCleanupExecutor) run(ctx context.Context, logger io.Writer, params map[string]string) (appliedConfigTZCleanupResult, error) {
	dryRun := params["dryRun"] == "true"
	if dryRun {
		_, _ = fmt.Fprintln(logger, "DRY RUN: no changes will be made.")
	}

	result := appliedConfigTZCleanupResult{}
	cursor := ""
	const pageSize = 200

	for {
		page, err := e.Store.ListAgents(ctx, store.AgentFilter{IncludeDeleted: true}, store.ListOptions{
			Limit:          pageSize,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if err != nil {
			return result, fmt.Errorf("list agents: %w", err)
		}

		for i := range page.Items {
			agent := &page.Items[i]
			result.AgentsScanned++

			if dryRun {
				// The page copy is never written back, so adopting on it
				// in memory only reports what a real run would do.
				e.record(logger, &result, agent.ID, adoptLegacyTZOutcome(agent.AppliedConfig), true)
				continue
			}
			if !appliedConfigHasEnvTZ(agent.AppliedConfig) {
				continue
			}
			outcome, err := e.adoptWithRetry(ctx, agent.ID)
			if err != nil {
				_, _ = fmt.Fprintf(logger, "  WARN agent=%s - failed to update: %v\n", agent.ID, err)
				continue
			}
			e.record(logger, &result, agent.ID, outcome, false)
		}

		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	verb := "Adopted"
	if dryRun {
		verb = "Would adopt"
	}
	_, _ = fmt.Fprintf(logger, "Scanned %d agent(s). %s %d agent TZ value(s) as legacy pins; stripped TZ from %d agent(s) without a new pin.\n",
		result.AgentsScanned, verb, result.AgentsAdopted, result.AgentsStripped)
	return result, nil
}

// record logs one agent's outcome and adds it to result.
func (e *AppliedConfigTZCleanupExecutor) record(logger io.Writer, result *appliedConfigTZCleanupResult, agentID string, outcome tzAdoptOutcome, dryRun bool) {
	prefix := ""
	if dryRun {
		prefix = "WOULD "
	}
	switch outcome {
	case tzAdoptAdopted:
		result.AgentsAdopted++
		_, _ = fmt.Fprintf(logger, "  %sADOPT agent=%s source=%s\n", prefix, agentID, TZSourceLegacy)
	case tzAdoptStripped:
		result.AgentsStripped++
		_, _ = fmt.Fprintf(logger, "  %sSTRIP agent=%s key=%s\n", prefix, agentID, agentTZEnvKey)
	}
}

// appliedConfigHasEnvTZ reports whether either env copy that adoptLegacyTZ
// strips holds a TZ key, including an empty one.
func appliedConfigHasEnvTZ(ac *store.AgentAppliedConfig) bool {
	if ac == nil {
		return false
	}
	if _, ok := ac.Env[agentTZEnvKey]; ok {
		return true
	}
	if ac.InlineConfig != nil {
		if _, ok := ac.InlineConfig.Env[agentTZEnvKey]; ok {
			return true
		}
	}
	return false
}

// adoptWithRetry re-reads the agent, runs adoptLegacyTZ and writes the row,
// retrying on an optimistic-lock conflict against the latest row. The
// outcome is that of the attempt that was written (or tzAdoptNone when a
// concurrent writer already adopted). A row that keeps losing the race is
// picked up by the next run.
func (e *AppliedConfigTZCleanupExecutor) adoptWithRetry(ctx context.Context, agentID string) (tzAdoptOutcome, error) {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		agent, err := e.Store.GetAgent(ctx, agentID)
		if err != nil {
			return tzAdoptNone, err
		}
		outcome := adoptLegacyTZOutcome(agent.AppliedConfig)
		if outcome == tzAdoptNone {
			return tzAdoptNone, nil
		}
		err = e.Store.UpdateAgent(ctx, agent)
		if err == nil {
			return outcome, nil
		}
		if !errors.Is(err, store.ErrVersionConflict) {
			return tzAdoptNone, err
		}
	}
	return tzAdoptNone, fmt.Errorf("agent %s: gave up after %d version-conflict retries", agentID, maxAttempts)
}
