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

package entadapter

import (
	"context"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/group"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/project"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/schedule"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/user"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ListFixtureRootedResources returns every agent (soft-deleted included),
// schedule, group and project whose fixture_root_id is rootID. The stamp
// column is the only selector (ptone/scion#4240, Phase 3b); see
// store.Store.ListFixtureRootedResources.
func (c *CompositeStore) ListFixtureRootedResources(ctx context.Context, rootID string, limit int) (*store.FixtureRootedResources, error) {
	root, err := parseUUID(rootID)
	if err != nil {
		return nil, err
	}
	out := &store.FixtureRootedResources{}
	// fetch is limit+1 so a full page is told apart from a truncated one.
	fetch := 0
	if limit > 0 {
		fetch = limit + 1
	}
	trim := func(n int) int {
		if limit > 0 && n > limit {
			out.Truncated = true
			return limit
		}
		return n
	}

	aq := c.client.Agent.Query().Where(agent.FixtureRootIDEQ(root)).Order(agent.ByID())
	if fetch > 0 {
		aq = aq.Limit(fetch)
	}
	agents, err := aq.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	for _, a := range agents[:trim(len(agents))] {
		out.Agents = append(out.Agents, *entAgentToStore(a))
	}

	sq := c.client.Schedule.Query().Where(schedule.FixtureRootIDEQ(root)).Order(schedule.ByID())
	if fetch > 0 {
		sq = sq.Limit(fetch)
	}
	schedules, err := sq.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	for _, sc := range schedules[:trim(len(schedules))] {
		out.Schedules = append(out.Schedules, *entScheduleToStore(sc))
	}

	gq := c.client.Group.Query().Where(group.FixtureRootIDEQ(root)).Order(group.ByID())
	if fetch > 0 {
		gq = gq.Limit(fetch)
	}
	groups, err := gq.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	for _, g := range groups[:trim(len(groups))] {
		out.Groups = append(out.Groups, *entGroupToStore(g))
	}

	pq := c.client.Project.Query().Where(project.FixtureRootIDEQ(root)).Order(project.ByID())
	if fetch > 0 {
		pq = pq.Limit(fetch)
	}
	projects, err := pq.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	for _, p := range projects[:trim(len(projects))] {
		out.Projects = append(out.Projects, *entProjectToStore(p))
	}
	return out, nil
}

// ListExpiredTestFixtureUsers returns the test fixture users q selects,
// ordered by (expires_at, id); see store.ExpiredTestFixtureQuery.
func (s *UserStore) ListExpiredTestFixtureUsers(ctx context.Context, q store.ExpiredTestFixtureQuery) ([]store.User, error) {
	if q.ExpiredAtOrBefore.IsZero() {
		return nil, fmt.Errorf("%w: ExpiredAtOrBefore is required", store.ErrInvalidInput)
	}
	query := s.client.User.Query().
		Where(user.KindEQ(user.KindTestFixture), user.ExpiresAtNotNil(), user.ExpiresAtLTE(q.ExpiredAtOrBefore))
	if !q.ExpiredAfter.IsZero() {
		query = query.Where(user.ExpiresAtGT(q.ExpiredAfter))
	}
	if q.AfterID != "" {
		afterID, err := parseUUID(q.AfterID)
		if err != nil {
			return nil, err
		}
		query = query.Where(user.Or(
			user.ExpiresAtGT(q.AfterExpiresAt),
			user.And(user.ExpiresAtEQ(q.AfterExpiresAt), user.IDGT(afterID)),
		))
	}
	query = query.Order(user.ByExpiresAt(), user.ByID())
	if q.Limit > 0 {
		query = query.Limit(q.Limit)
	}
	rows, err := query.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.User, 0, len(rows))
	for _, r := range rows {
		out = append(out, *entUserToStore(r))
	}
	return out, nil
}
