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

package artifacts

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The sponsor backfill (ptone/scion#6028): artifacts created before the
// delegating user was recorded at creation get it once, from the agent's
// delegation chain as it resolves when the backfill runs. That is the
// agent's delegating user at backfill time, which for an agent re-delegated
// since may differ from the one at the artifact's creation; it is the best
// value still available. Agents already deleted resolve to no one, and
// their artifacts keep nothing recorded.

// SponsorBackfillResult reports a BackfillSponsors call.
type SponsorBackfillResult struct {
	// AlreadyDone is true when the ledger already recorded the backfill;
	// nothing else was read or changed.
	AlreadyDone bool
	// Agents counts the agents with unrecorded artifacts the pass looked
	// up, Resolved those the resolver named a user for, and Artifacts the
	// artifacts it recorded a sponsor on.
	Agents    int
	Resolved  int
	Artifacts int
	// Done is true when the pass completed and recorded the ledger name.
	Done bool
}

// BackfillSponsors records the delegating user on the existing artifacts
// of agents that have none recorded (ptone/scion#6028), once per database.
// It pages through the agents owning a live project-scope artifact with no
// sponsor (batch at a time), asks resolve for each agent once, and records
// a non-empty answer on that agent's unrecorded artifacts; it never
// changes a recorded sponsor or touches a deleted artifact.
//
// resolve follows SponsorRecorder.DelegatingUser's contract: ("", nil)
// means the agent has no delegating user (a deleted agent, or one whose
// chain does not resolve), and its artifacts keep nothing recorded, so the
// live chain keeps deciding for them; an error means the lookup failed.
// When every page is done the pass records migrationSponsorBackfill, after
// which a call returns at once. A resolve or store error stops the pass
// without recording it, so the next call starts again; what it recorded
// so far stays.
func BackfillSponsors(ctx context.Context, st Store, resolve func(ctx context.Context, agentID string) (string, error), batch int, now time.Time) (SponsorBackfillResult, error) {
	var res SponsorBackfillResult
	if st == nil || resolve == nil {
		return res, errors.New("artifacts: BackfillSponsors needs a store and a resolver")
	}
	if batch <= 0 {
		return res, errors.New("artifacts: BackfillSponsors needs a positive batch")
	}
	done, err := st.SponsorBackfillDone(ctx)
	if err != nil {
		return res, err
	}
	if done {
		res.AlreadyDone = true
		return res, nil
	}
	after := ""
	for {
		agents, err := st.ListUnsponsoredAgents(ctx, after, batch)
		if err != nil {
			return res, err
		}
		for _, agentID := range agents {
			res.Agents++
			user, err := resolve(ctx, agentID)
			if err != nil {
				return res, fmt.Errorf("artifacts: sponsor backfill: resolve agent %s: %w", agentID, err)
			}
			if user == "" {
				continue
			}
			res.Resolved++
			n, err := st.SetAgentSponsor(ctx, agentID, user)
			if err != nil {
				return res, err
			}
			res.Artifacts += n
		}
		if len(agents) < batch {
			break
		}
		after = agents[len(agents)-1]
	}
	if err := st.MarkSponsorBackfillDone(ctx, now); err != nil {
		return res, err
	}
	res.Done = true
	return res, nil
}

// SponsorBackfillDone implements Store.
func (s *sqlStore) SponsorBackfillDone(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, s.rebind("SELECT COUNT(*) FROM artifact_migrations WHERE name = ?"),
		migrationSponsorBackfill).Scan(&n); err != nil {
		return false, fmt.Errorf("artifacts: read migrations ledger: %w", err)
	}
	return n > 0, nil
}

// ListUnsponsoredAgents implements Store.
func (s *sqlStore) ListUnsponsoredAgents(ctx context.Context, after string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, errors.New("artifacts: ListUnsponsoredAgents needs a positive limit")
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT DISTINCT owner_ref FROM artifact
		WHERE owner_kind = ? AND scope_kind = ? AND sponsor_ref IS NULL AND deleted_at IS NULL AND owner_ref > ?
		ORDER BY owner_ref LIMIT ?`), PrincipalKindAgent, ScopeKindProject, after, limit)
	if err != nil {
		return nil, fmt.Errorf("artifacts: list unsponsored agents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("artifacts: scan unsponsored agent: %w", err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: list unsponsored agents: %w", err)
	}
	return out, nil
}

// SetAgentSponsor implements Store.
func (s *sqlStore) SetAgentSponsor(ctx context.Context, agentID, userID string) (int, error) {
	if agentID == "" || userID == "" {
		return 0, errors.New("artifacts: SetAgentSponsor needs an agent and a user")
	}
	r, err := s.db.ExecContext(ctx, s.rebind(`UPDATE artifact SET sponsor_ref = ?
		WHERE owner_kind = ? AND owner_ref = ? AND scope_kind = ? AND sponsor_ref IS NULL AND deleted_at IS NULL`),
		userID, PrincipalKindAgent, agentID, ScopeKindProject)
	if err != nil {
		return 0, fmt.Errorf("artifacts: record sponsor: %w", err)
	}
	n, err := r.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("artifacts: record sponsor: %w", err)
	}
	return int(n), nil
}

// MarkSponsorBackfillDone implements Store.
func (s *sqlStore) MarkSponsorBackfillDone(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, s.rebind(`INSERT INTO artifact_migrations (name, applied_at) VALUES (?, ?)
		ON CONFLICT (name) DO NOTHING`), migrationSponsorBackfill, s.timeArg(now)); err != nil {
		return fmt.Errorf("artifacts: record migration %s: %w", migrationSponsorBackfill, err)
	}
	return nil
}
