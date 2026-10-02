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
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// scriptedListerCallBudget bounds how many times a scriptedAgentLister
// answers. A walker that loops on a cursor cycle then fails with a budget
// error instead of hanging the test.
const scriptedListerCallBudget = 50

// scriptedAgentLister serves canned pages keyed by the cursor it is asked
// for, and records every cursor it was called with and whether the call
// asked the store to skip the total count. Calls past
// scriptedListerCallBudget return an error.
type scriptedAgentLister struct {
	pages     map[string]store.ListResult[store.Agent]
	cursors   []string
	skipTotal []bool
}

func (l *scriptedAgentLister) ListAgents(_ context.Context, _ store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	if len(l.cursors) >= scriptedListerCallBudget {
		return nil, fmt.Errorf("scripted lister call budget of %d exceeded", scriptedListerCallBudget)
	}
	l.cursors = append(l.cursors, opts.Cursor)
	l.skipTotal = append(l.skipTotal, opts.SkipTotalCount)
	page, ok := l.pages[opts.Cursor]
	if !ok {
		return nil, fmt.Errorf("unexpected cursor %q", opts.Cursor)
	}
	return &page, nil
}

func scriptedPage(prefix string, n int, next string) store.ListResult[store.Agent] {
	items := make([]store.Agent, n)
	for i := range items {
		items[i] = store.Agent{ID: fmt.Sprintf("%s-%d", prefix, i)}
	}
	return store.ListResult[store.Agent]{Items: items, NextCursor: next}
}

// captureSpaceMembersLogs routes the default slog logger (warnings and
// above) into a buffer for the duration of the test.
//
// It swaps package-level state, so it must not be used from parallel tests.
func captureSpaceMembersLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	prevLogger := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	return &buf
}

// setSpaceMembersMaxAgents overrides the members cap for the duration of the
// test.
//
// It swaps package-level state, so it must not be used from parallel tests.
func setSpaceMembersMaxAgents(t *testing.T, maxAgents int) {
	t.Helper()
	prevCap := spaceMembersMaxAgents
	spaceMembersMaxAgents = maxAgents
	t.Cleanup(func() { spaceMembersMaxAgents = prevCap })
}

const spaceMembersCapWarning = "agent list truncated at safety cap"

func TestProjectAgentWalk_RepeatedCursorErrors(t *testing.T) {
	lister := &scriptedAgentLister{pages: map[string]store.ListResult[store.Agent]{
		"":  scriptedPage("p1", 1, "a"),
		"a": scriptedPage("p2", 1, "a"),
	}}
	agents, truncated, err := walkProjectAgentPages(context.Background(), lister, "proj", 50)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repeated cursor")
	assert.Nil(t, agents)
	assert.False(t, truncated)
	assert.Equal(t, []string{"", "a"}, lister.cursors)
}

// Without a cap nothing else bounds the walk, so a cursor cycle longer than
// one page must still be caught.
func TestProjectAgentWalk_CursorCycleErrorsWithoutCap(t *testing.T) {
	lister := &scriptedAgentLister{pages: map[string]store.ListResult[store.Agent]{
		"":  scriptedPage("p1", 1, "a"),
		"a": scriptedPage("p2", 1, "b"),
		"b": scriptedPage("p3", 1, "a"),
	}}
	agents, _, err := walkProjectAgentPages(context.Background(), lister, "proj", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repeated cursor")
	assert.Nil(t, agents)
	assert.Equal(t, []string{"", "a", "b"}, lister.cursors)
}

func TestProjectAgentWalk_NoCapReturnsEveryPage(t *testing.T) {
	lister := &scriptedAgentLister{pages: map[string]store.ListResult[store.Agent]{
		"":  scriptedPage("p1", 3, "a"),
		"a": scriptedPage("p2", 3, "b"),
		"b": scriptedPage("p3", 2, ""),
	}}
	agents, truncated, err := walkProjectAgentPages(context.Background(), lister, "proj", 0)
	require.NoError(t, err)
	assert.Len(t, agents, 8)
	assert.False(t, truncated)
	assert.Equal(t, []string{"", "a", "b"}, lister.cursors)
}

func TestProjectAgentWalk_SkipsTotalCountOnEveryPage(t *testing.T) {
	lister := &scriptedAgentLister{pages: map[string]store.ListResult[store.Agent]{
		"":  scriptedPage("p1", 1, "a"),
		"a": scriptedPage("p2", 1, ""),
	}}
	_, _, err := walkProjectAgentPages(context.Background(), lister, "proj", 0)
	require.NoError(t, err)
	assert.Equal(t, []bool{true, true}, lister.skipTotal)
}

func TestProjectAgentWalk_CancelledContextStopsBeforeListing(t *testing.T) {
	lister := &scriptedAgentLister{pages: map[string]store.ListResult[store.Agent]{
		"": scriptedPage("p1", 1, ""),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	agents, _, err := walkProjectAgentPages(ctx, lister, "proj", 50)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, agents)
	assert.Empty(t, lister.cursors, "ListAgents must not be called with a cancelled context")
}

func TestProjectAgentWalk_TotalEqualToCapReturnsAllNotTruncated(t *testing.T) {
	lister := &scriptedAgentLister{pages: map[string]store.ListResult[store.Agent]{
		"":  scriptedPage("p1", 2, "a"),
		"a": scriptedPage("p2", 2, ""),
	}}
	agents, truncated, err := walkProjectAgentPages(context.Background(), lister, "proj", 4)
	require.NoError(t, err)
	assert.Len(t, agents, 4)
	assert.False(t, truncated)
	assert.Equal(t, []string{"", "a"}, lister.cursors, "no page may be read after the final one")
}

func TestProjectAgentWalk_ReachingCapStopsWithoutReadingNextPage(t *testing.T) {
	lister := &scriptedAgentLister{pages: map[string]store.ListResult[store.Agent]{
		"":  scriptedPage("p1", 4, "a"),
		"a": scriptedPage("p2", 4, ""),
	}}
	agents, truncated, err := walkProjectAgentPages(context.Background(), lister, "proj", 4)
	require.NoError(t, err)
	assert.Len(t, agents, 4)
	assert.True(t, truncated)
	assert.Equal(t, []string{""}, lister.cursors, "the walk must stop once the cap is reached")
}

func TestProjectAgentWalk_PastCapTruncates(t *testing.T) {
	lister := &scriptedAgentLister{pages: map[string]store.ListResult[store.Agent]{
		"":  scriptedPage("p1", 3, "a"),
		"a": scriptedPage("p2", 3, ""),
	}}
	agents, truncated, err := walkProjectAgentPages(context.Background(), lister, "proj", 4)
	require.NoError(t, err)
	require.Len(t, agents, 4)
	assert.True(t, truncated)
	assert.Equal(t, "p2-0", agents[3].ID)
	assert.Equal(t, []string{"", "a"}, lister.cursors)
}
