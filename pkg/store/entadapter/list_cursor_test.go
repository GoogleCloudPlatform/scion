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
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// malformedListCursors are cursors decodeListCursor must reject, one per
// failure branch.
func malformedListCursors() map[string]string {
	enc := func(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	return map[string]string{
		"not base64":        "not-base64-!!!",
		"padding only":      "====",
		"too few parts":     enc("not-enough-parts"),
		"bad timestamp":     enc("not-a-timestamp," + uuid.NewString()),
		"bad id":            enc(ts + ",not-a-uuid"),
		"unexpected suffix": enc(ts + "," + uuid.NewString() + ",some-binding"),
	}
}

// TestDecodeListCursor_ErrorsAreInvalidInput pins the central contract
// (ptone/scion#1957): every decodeListCursor failure wraps
// store.ErrInvalidInput, so the hub maps a malformed cursor to 400.
func TestDecodeListCursor_ErrorsAreInvalidInput(t *testing.T) {
	for name, cursor := range malformedListCursors() {
		t.Run(name, func(t *testing.T) {
			_, _, err := decodeListCursor(cursor, "")
			require.Error(t, err)
			assert.ErrorIs(t, err, store.ErrInvalidInput)
		})
	}

	t.Run("binding mismatch", func(t *testing.T) {
		cursor := encodeListCursor(time.Now(), uuid.NewString(), "binding-a")
		_, _, err := decodeListCursor(cursor, "binding-b")
		require.Error(t, err)
		assert.ErrorIs(t, err, store.ErrInvalidInput)
	})

	t.Run("valid cursor round-trips", func(t *testing.T) {
		created := time.Now().UTC().Truncate(time.Microsecond)
		id := uuid.New()
		gotCreated, gotID, err := decodeListCursor(encodeListCursor(created, id.String(), "b"), "b")
		require.NoError(t, err)
		assert.True(t, created.Equal(gotCreated))
		assert.Equal(t, id, gotID)
	})
}

// TestDecodeCursor_ErrorsAreInvalidInput is decodeListCursor's contract for
// the unbound decodeCursor used by messages, conversations and schedules.
func TestDecodeCursor_ErrorsAreInvalidInput(t *testing.T) {
	enc := func(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	for name, cursor := range map[string]string{
		"not base64":    "not-base64-!!!",
		"padding only":  "====",
		"too few parts": enc("not-enough-parts"),
		"bad timestamp": enc("not-a-timestamp," + uuid.NewString()),
		"bad id":        enc(ts + ",not-a-uuid"),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := decodeCursor(cursor)
			require.Error(t, err)
			assert.ErrorIs(t, err, store.ErrInvalidInput)
		})
	}

	t.Run("valid cursor round-trips", func(t *testing.T) {
		created := time.Now().UTC().Truncate(time.Microsecond)
		id := uuid.New()
		gotCreated, gotID, err := decodeCursor(encodeCursor(created, id.String()))
		require.NoError(t, err)
		assert.True(t, created.Equal(gotCreated))
		assert.Equal(t, id, gotID)
	})
}

// TestListStores_MalformedCursorIsInvalidInput hits every paginated store
// list method (decodeListCursor, decodeCursor and UUID cursors) and asserts the error
// surfaces as store.ErrInvalidInput (HTTP 400 at the hub), not a bare error
// (HTTP 500). ptone/scion#1957.
func TestListStores_MalformedCursorIsInvalidInput(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	lists := map[string]func(opts store.ListOptions) error{
		"agents": func(opts store.ListOptions) error {
			_, err := cs.ListAgents(ctx, store.AgentFilter{}, opts)
			return err
		},
		"projects": func(opts store.ListOptions) error {
			_, err := cs.ListProjects(ctx, store.ProjectFilter{}, opts)
			return err
		},
		"runtime brokers": func(opts store.ListOptions) error {
			_, err := cs.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, opts)
			return err
		},
		"templates": func(opts store.ListOptions) error {
			_, err := cs.ListTemplates(ctx, store.TemplateFilter{}, opts)
			return err
		},
		"harness configs": func(opts store.ListOptions) error {
			_, err := cs.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, opts)
			return err
		},
		"groups": func(opts store.ListOptions) error {
			_, err := cs.ListGroups(ctx, store.GroupFilter{}, opts)
			return err
		},
		"skills": func(opts store.ListOptions) error {
			_, err := cs.ListSkills(ctx, store.SkillFilter{}, opts)
			return err
		},
		// decodeCursor (no binding) callers.
		"messages": func(opts store.ListOptions) error {
			_, err := cs.ListMessages(ctx, store.MessageFilter{}, opts)
			return err
		},
		"conversations": func(opts store.ListOptions) error {
			_, err := cs.ListConversations(ctx, store.ConversationFilter{}, opts)
			return err
		},
		"schedules": func(opts store.ListOptions) error {
			_, err := cs.ListSchedules(ctx, store.ScheduleFilter{}, opts)
			return err
		},
		// UUID cursor (parseUUID), included so every paginated list is covered.
		"scheduled events": func(opts store.ListOptions) error {
			_, err := cs.ListScheduledEvents(ctx, store.ScheduledEventFilter{}, opts)
			return err
		},
	}

	for listName, list := range lists {
		for cursorName, cursor := range malformedListCursors() {
			t.Run(listName+"/"+cursorName, func(t *testing.T) {
				err := list(store.ListOptions{Limit: 5, Cursor: cursor})
				require.Error(t, err)
				assert.ErrorIs(t, err, store.ErrInvalidInput)
			})
		}
	}
}
