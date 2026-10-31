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

package storetest

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ScheduleAuthorityFilterConformance verifies ScheduleFilter.AuthorityPrincipal
// (ptone/scion#2143) against a backend: it keeps the schedules whose
// recorded authority is the principal, across projects, and never matches
// on CreatedBy, on a legacy attribution or on the same ID under another
// principal kind. It combines with the project and status filters, its
// TotalCount covers only the matching rows, and a principal with a kind but
// no ID (or the reverse) is refused.
func ScheduleAuthorityFilterConformance(t *testing.T, factory Factory) {
	t.Helper()
	ctx := context.Background()
	s := factory(t)

	userID := uuid.NewString()
	otherUserID := uuid.NewString()
	projectA := uuid.NewString()
	projectB := uuid.NewString()

	recorded := func(kind, id, cred string) store.InitiatorAttribution {
		return store.InitiatorAttribution{
			InitiatorPrincipalKind:  kind,
			InitiatorPrincipalID:    id,
			InitiatorCredentialKind: cred,
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		}
	}
	legacyVersion := recorded(store.DelegationPrincipalUser, userID, store.InitiatorCredentialKindSession)
	legacyVersion.AttributionVersion = 0
	legacyVersion.AuthorizationRevision = 0

	type row struct {
		name      string
		projectID string
		status    string
		createdBy string
		attr      store.InitiatorAttribution
	}
	rows := []row{
		// The user's recorded authority, in two projects and both
		// user credential kinds.
		{"held-session", projectA, store.ScheduleStatusActive, otherUserID, recorded(store.DelegationPrincipalUser, userID, store.InitiatorCredentialKindSession)},
		{"held-uat", projectB, store.ScheduleStatusActive, userID, recorded(store.DelegationPrincipalUser, userID, store.InitiatorCredentialKindUAT)},
		{"held-paused", projectA, store.ScheduleStatusPaused, userID, recorded(store.DelegationPrincipalUser, userID, store.InitiatorCredentialKindSession)},
		{"held-deleted", projectA, store.ScheduleStatusDeleted, userID, recorded(store.DelegationPrincipalUser, userID, store.InitiatorCredentialKindSession)},
		// Created by the user, but the authority was re-recorded to
		// another user: not the user's.
		{"created-only", projectA, store.ScheduleStatusActive, userID, recorded(store.DelegationPrincipalUser, otherUserID, store.InitiatorCredentialKindSession)},
		{"legacy-version", projectA, store.ScheduleStatusActive, userID, legacyVersion},
		{"legacy-cred", projectA, store.ScheduleStatusActive, userID, recorded(store.DelegationPrincipalUser, userID, store.InitiatorCredentialKindLegacyUnknown)},
		{"blank-cred", projectA, store.ScheduleStatusActive, userID, recorded(store.DelegationPrincipalUser, userID, "")},
		{"agent-same-id", projectA, store.ScheduleStatusActive, userID, recorded(store.DelegationPrincipalAgent, userID, store.InitiatorCredentialKindAgent)},
		{"none", projectB, store.ScheduleStatusActive, userID, store.InitiatorAttribution{}},
	}
	ids := map[string]string{}
	for _, r := range rows {
		next := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		sc := &store.Schedule{
			ID:                   uuid.NewString(),
			ProjectID:            r.projectID,
			Name:                 "authority-" + r.name,
			CronExpr:             "0 9 * * *",
			EventType:            "message",
			Payload:              `{"message":"hi"}`,
			Status:               r.status,
			NextRunAt:            &next,
			CreatedBy:            r.createdBy,
			InitiatorAttribution: r.attr,
		}
		require.NoError(t, s.CreateSchedule(ctx, sc), r.name)
		ids[sc.ID] = r.name
	}

	listAll := func(t *testing.T, f store.ScheduleFilter) ([]string, int) {
		t.Helper()
		var names []string
		total := -1
		opts := store.ListOptions{Limit: 1}
		for {
			page, err := s.ListSchedules(ctx, f, opts)
			require.NoError(t, err)
			if total < 0 {
				total = page.TotalCount
			}
			for _, sc := range page.Items {
				names = append(names, ids[sc.ID])
			}
			if page.NextCursor == "" {
				break
			}
			require.NotEqual(t, opts.Cursor, page.NextCursor, "paging advances")
			opts.Cursor = page.NextCursor
		}
		sort.Strings(names)
		return names, total
	}
	user := store.ScheduleAuthorityPrincipal{Kind: store.DelegationPrincipalUser, ID: userID}

	t.Run("AcrossProjects", func(t *testing.T) {
		names, total := listAll(t, store.ScheduleFilter{AuthorityPrincipal: user})
		assert.Equal(t, []string{"held-paused", "held-session", "held-uat"}, names,
			"every non-deleted schedule whose recorded authority is the user, in any project")
		assert.Equal(t, 3, total)
	})

	t.Run("ActiveOnly", func(t *testing.T) {
		names, total := listAll(t, store.ScheduleFilter{AuthorityPrincipal: user, Status: store.ScheduleStatusActive})
		assert.Equal(t, []string{"held-session", "held-uat"}, names)
		assert.Equal(t, 2, total)
	})

	t.Run("WithProject", func(t *testing.T) {
		names, total := listAll(t, store.ScheduleFilter{AuthorityPrincipal: user, ProjectID: projectB})
		assert.Equal(t, []string{"held-uat"}, names)
		assert.Equal(t, 1, total)
	})

	t.Run("KindIsPartOfThePrincipal", func(t *testing.T) {
		names, total := listAll(t, store.ScheduleFilter{
			AuthorityPrincipal: store.ScheduleAuthorityPrincipal{Kind: store.DelegationPrincipalAgent, ID: userID},
		})
		assert.Equal(t, []string{"agent-same-id"}, names)
		assert.Equal(t, 1, total)
	})

	t.Run("OtherPrincipal", func(t *testing.T) {
		names, _ := listAll(t, store.ScheduleFilter{
			AuthorityPrincipal: store.ScheduleAuthorityPrincipal{Kind: store.DelegationPrincipalUser, ID: otherUserID},
		})
		assert.Equal(t, []string{"created-only"}, names, "the principal the authority was re-recorded to")
	})

	t.Run("ZeroValueIsNoFilter", func(t *testing.T) {
		names, _ := listAll(t, store.ScheduleFilter{})
		assert.Len(t, names, len(rows)-1, "every row but the deleted one")
	})

	t.Run("HalfSetRefused", func(t *testing.T) {
		for _, p := range []store.ScheduleAuthorityPrincipal{
			{Kind: store.DelegationPrincipalUser},
			{ID: userID},
		} {
			_, err := s.ListSchedules(ctx, store.ScheduleFilter{AuthorityPrincipal: p}, store.ListOptions{Limit: 10})
			assert.ErrorIs(t, err, store.ErrInvalidInput, "%+v", p)
		}
	})
}
