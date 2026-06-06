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

//go:build integration

// Category 6 — Schema / type edge cases. Postgres is strictly typed where SQLite
// is loose, so these pin behaviors that can silently differ between the two
// backends: NULL semantics for nullable columns, exact round-tripping of
// Unicode/emoji and JSON (including nested structures and special characters),
// large text values that must not be truncated, and TIMESTAMPTZ microsecond
// precision (vs SQLite's text timestamps).
package integrationtest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestSchema_NullableRoundTrip verifies nullable columns store and read back as
// genuine SQL NULL (not a zero-value sentinel) and transition correctly when set.
func TestSchema_NullableRoundTrip(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	db := cs.DB()
	require.NotNil(t, db)
	project := seedProject(t, cs)

	// A pending scheduled event has a NULL fired_at.
	evt := makeScheduledEvent(project.ID)
	require.NoError(t, cs.CreateScheduledEvent(ctx, evt))
	got, err := cs.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	assert.Nil(t, got.FiredAt, "pending event must have nil FiredAt")

	var firedIsNull bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT fired_at IS NULL FROM scheduled_events WHERE id=$1`, evt.ID).Scan(&firedIsNull))
	assert.True(t, firedIsNull, "fired_at must be SQL NULL, not a zero timestamp")

	// Claiming sets fired_at to a real value.
	won, err := cs.ClaimScheduledEvent(ctx, evt.ID, store.ScheduledEventFired)
	require.NoError(t, err)
	require.True(t, won)
	got, err = cs.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	require.NotNil(t, got.FiredAt, "claimed event must have a non-nil FiredAt")

	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT fired_at IS NULL FROM scheduled_events WHERE id=$1`, evt.ID).Scan(&firedIsNull))
	assert.False(t, firedIsNull, "fired_at must no longer be NULL after a claim")

	// An agent created without an owner has a NULL owner_id (not an empty string).
	ag := makeAgent(project.ID, "null-owner-"+shortID())
	require.NoError(t, cs.CreateAgent(ctx, ag))
	var ownerIsNull bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT owner_id IS NULL FROM agents WHERE id=$1`, ag.ID).Scan(&ownerIsNull))
	assert.True(t, ownerIsNull, "unset owner_id must be SQL NULL")
	reread, err := cs.GetAgent(ctx, ag.ID)
	require.NoError(t, err)
	assert.Equal(t, "", reread.OwnerID, "NULL owner_id must read back as empty string")
}

// TestSchema_UnicodeAndEmoji verifies multibyte Unicode and emoji round-trip
// byte-for-byte through text columns.
func TestSchema_UnicodeAndEmoji(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()

	const fancy = "项目 🚀 café ☃ Ω≈ç √∫ — “quotes” 𝔘𝔫𝔦𝔠𝔬𝔡𝔢"
	p := makeProject("unicode-" + shortID())
	p.Name = fancy
	require.NoError(t, cs.CreateProject(ctx, p))

	got, err := cs.GetProject(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, fancy, got.Name, "Unicode/emoji must round-trip exactly")
}

// TestSchema_NestedJSONAndSpecialChars verifies JSON-bearing columns preserve
// nested objects, arrays, and special characters exactly. ScheduledEvent.Payload
// is stored verbatim; Agent.Labels is marshaled to a JSON column.
func TestSchema_NestedJSONAndSpecialChars(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	project := seedProject(t, cs)

	// Verbatim JSON text column.
	const payload = `{"nested":{"arr":[1,2,3],"flag":true,"s":"emoji 🎉 \"quoted\" back\\slash tab\there","null":null},"unicode":"café"}`
	evt := makeScheduledEvent(project.ID)
	evt.Payload = payload
	require.NoError(t, cs.CreateScheduledEvent(ctx, evt))
	gotEvt, err := cs.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	assert.Equal(t, payload, gotEvt.Payload, "nested JSON payload must round-trip verbatim")

	// JSON-marshaled map column with special-character values.
	labels := map[string]string{
		"emoji":   "🔥💧",
		"quotes":  `he said "hi"`,
		"unicode": "naïve café",
		"nl":      "line1\nline2\ttabbed",
	}
	ag := makeAgent(project.ID, "json-labels-"+shortID())
	ag.Labels = labels
	require.NoError(t, cs.CreateAgent(ctx, ag))
	gotAg, err := cs.GetAgent(ctx, ag.ID)
	require.NoError(t, err)
	assert.Equal(t, labels, gotAg.Labels, "JSON label map must round-trip exactly, special chars included")
}

// TestSchema_LargeTextNoTruncation verifies a large string in a text column is
// stored and returned without truncation. Ent maps Go strings to unbounded
// Postgres TEXT, so there is no VARCHAR(n) boundary to silently clip at.
func TestSchema_LargeTextNoTruncation(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	project := seedProject(t, cs)

	large := strings.Repeat("A", 100*1024) // 100 KiB
	evt := makeScheduledEvent(project.ID)
	evt.Payload = large
	require.NoError(t, cs.CreateScheduledEvent(ctx, evt))

	got, err := cs.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	assert.Equal(t, len(large), len(got.Payload), "large text must not be truncated")
	assert.Equal(t, large, got.Payload)
}

// TestSchema_TimestampPrecision verifies TIMESTAMPTZ preserves sub-second
// precision to the microsecond (Postgres' resolution) and a stable instant.
// Nanoseconds below the microsecond are truncated by Postgres — that truncation
// is the documented behavior, not data loss to the second as a naive text-based
// (SQLite) representation might do.
func TestSchema_TimestampPrecision(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	project := seedProject(t, cs)

	// A time carrying both microsecond and stray nanosecond components.
	in := time.Date(2026, 6, 2, 13, 14, 15, 123456789, time.UTC)
	wantMicro := in.Truncate(time.Microsecond) // 123456000 ns

	evt := makeScheduledEvent(project.ID)
	evt.FireAt = in
	require.NoError(t, cs.CreateScheduledEvent(ctx, evt))

	got, err := cs.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)

	assert.True(t, got.FireAt.UTC().Equal(wantMicro),
		"fire_at must preserve the microsecond instant: got %v, want %v", got.FireAt.UTC(), wantMicro)
	assert.NotEqual(t, in.Truncate(time.Second), got.FireAt.UTC(),
		"sub-second precision must be retained (not truncated to whole seconds)")
	assert.Zero(t, got.FireAt.Nanosecond()%1000,
		"Postgres resolution is microseconds; nanosecond remainder must be zero")
}
