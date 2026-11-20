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

package triggerrule_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
)

func TestEnvVersion2_EventListsInCEL(t *testing.T) {
	assert.Equal(t, 2, triggerrule.EnvironmentVersion)
	assert.Equal(t, 2, triggerrule.Env().Version)

	rules, err := triggerrule.NewDefault()
	require.NoError(t, err)

	ctx := context.Background()
	scope := celengine.CacheScope{
		Consumer:  triggerrule.ConsumerName,
		ProjectID: "proj-1",
		ID:        "trig-1",
		Revision:  1,
	}

	ev := hubfire.FireEvent{
		ID:         "ev-1",
		ProjectID:  "proj-1",
		SourceKind: "github",
		SourceID:   "12345",
		Type:       "github.issue_comment.created",
		Subject:    "github:acme/widgets#42",
		OccurredAt: time.Now().UTC(),
		ReceivedAt: time.Now().UTC(),
		Attributes: map[string]string{
			"is_fork": "false",
		},
		Lists: map[string][]string{
			"labels":   {"bug", "triage:urgent"},
			"enriched": {"base_ref", "head_ref", "is_fork"},
		},
	}

	act, err := triggerrule.NewEventActivation(ev)
	require.NoError(t, err)

	// Mutating the caller's map/slices after activation creation must not leak.
	ev.Lists["labels"][0] = "mutated"
	ev.Lists["extra"] = []string{"x"}

	t.Run("event.lists.labels.exists matches", func(t *testing.T) {
		rule := triggerrule.Rule{
			Dialect: "cel",
			Expr:    "event.lists.labels.exists(l, l in ['bug', 'security'])",
		}
		require.NoError(t, rules.CompileMatch(ctx, rule))
		matched, err := rules.Match(ctx, scope, rule, act)
		require.NoError(t, err)
		assert.True(t, matched)
	})

	t.Run("event.lists.labels.exists non-match", func(t *testing.T) {
		rule := triggerrule.Rule{
			Dialect: "cel",
			Expr:    "event.lists.labels.exists(l, l == 'wontfix')",
		}
		matched, err := rules.Match(ctx, scope, rule, act)
		require.NoError(t, err)
		assert.False(t, matched)
	})

	t.Run("event.lists.enriched provenance check", func(t *testing.T) {
		rule := triggerrule.Rule{
			Dialect: "cel",
			Expr:    "has(event.lists.enriched) && 'is_fork' in event.lists.enriched && !has(event.lists.extra)",
		}
		matched, err := rules.Match(ctx, scope, rule, act)
		require.NoError(t, err)
		assert.True(t, matched)
	})

	t.Run("empty lists map exposes has() as false without error", func(t *testing.T) {
		emptyEv := hubfire.FireEvent{
			ID:         "ev-2",
			ProjectID:  "proj-1",
			SourceKind: "github",
			SourceID:   "12345",
			Type:       "github.issues.opened",
			Subject:    "github:acme/widgets#99",
			OccurredAt: time.Now().UTC(),
			ReceivedAt: time.Now().UTC(),
		}
		emptyAct, err := triggerrule.NewEventActivation(emptyEv)
		require.NoError(t, err)

		rule := triggerrule.Rule{
			Dialect: "cel",
			Expr:    "!has(event.lists.labels)",
		}
		matched, err := rules.Match(ctx, scope, rule, emptyAct)
		require.NoError(t, err)
		assert.True(t, matched)
	})
}
