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

//go:build !no_sqlite

package entadapter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestTriggerStore_UpdateTriggerEventAttributes(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	projID := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)

	ev := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "987654",
		Type:       "github.issue_comment.created",
		Subject:    "github:octo-org/octo-repo#42",
		DedupKey:   uuid.NewString() + "/" + projID,
		OccurredAt: now,
		ReceivedAt: now,
		Attributes: map[string]string{
			"event":           "issue_comment",
			"is_pull_request": "true",
		},
		Lists: map[string][]string{
			"labels": {"initial"},
		},
		Status: store.TriggerEventStatusPending,
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, ev))

	// 1. Happy path: updates attributes and lists on a pending event.
	updatedAttrs := map[string]string{
		"event":           "issue_comment",
		"is_pull_request": "true",
		"head_ref":        "feat/x",
		"is_fork":         "false",
		"enriched":        "true",
	}
	updatedLists := map[string][]string{
		"labels":   {"needs-review"},
		"enriched": {"head_ref", "is_fork"},
	}
	require.NoError(t, cs.UpdateTriggerEventAttributes(ctx, ev.ID, updatedAttrs, updatedLists))

	got, err := cs.GetTriggerEvent(ctx, ev.ID)
	require.NoError(t, err)
	assert.Equal(t, updatedAttrs, got.Attributes)
	assert.Equal(t, updatedLists, got.Lists)

	// 2. Oversized lists rejected with ErrInvalidInput.
	tooBig := map[string][]string{
		"labels": {strings.Repeat("x", store.MaxTriggerEventListsBytes+64)},
	}
	err = cs.UpdateTriggerEventAttributes(ctx, ev.ID, updatedAttrs, tooBig)
	assert.ErrorIs(t, err, store.ErrInvalidInput)

	// 3. Missing event ID returns ErrNotFound.
	err = cs.UpdateTriggerEventAttributes(ctx, uuid.NewString(), updatedAttrs, updatedLists)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// 4. Already-processed event is a no-op (does not overwrite).
	require.NoError(t, cs.MarkTriggerEventProcessed(ctx, ev.ID, now.Add(time.Second), 1, 1))
	require.NoError(t, cs.UpdateTriggerEventAttributes(ctx, ev.ID, map[string]string{"enriched": "failed"}, nil))
	gotAfter, err := cs.GetTriggerEvent(ctx, ev.ID)
	require.NoError(t, err)
	assert.Equal(t, "true", gotAfter.Attributes["enriched"])
}
