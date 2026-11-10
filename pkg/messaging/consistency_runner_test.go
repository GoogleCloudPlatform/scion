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

package messaging

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stalledQueryStore is a MessageQueryStore whose lookups block until their
// context is done or release is closed, whichever comes first.
type stalledQueryStore struct {
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (s *stalledQueryStore) ListMessages(ctx context.Context, _ store.MessageFilter, _ store.ListOptions) (*store.ListResult[store.Message], error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
		return &store.ListResult[store.Message]{}, nil
	}
}

func swapDivergenceMetrics(t *testing.T) {
	t.Helper()
	saved := DivergenceMetrics
	DivergenceMetrics = &DivergenceCounter{}
	t.Cleanup(func() { DivergenceMetrics = saved })
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

// A lookup that never answers fails open at ConsistencyCheckTimeout, even
// when the caller's context has no deadline, and the timeout is counted.
func TestCheckConversationConsistency_StalledLookupFailsOpenAtDeadline(t *testing.T) {
	for _, tc := range []struct {
		name, thread, sender, recipient string
	}{
		{name: "thread", thread: "t1"},
		{name: "pair", sender: "user-1", recipient: "agent-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			swapDivergenceMetrics(t)
			st := &stalledQueryStore{release: make(chan struct{})} // never released

			start := time.Now()
			ok := CheckConversationConsistency(context.Background(), st, "m1", "conv-1",
				tc.thread, tc.sender, tc.recipient, quietLogger())
			elapsed := time.Since(start)

			assert.True(t, ok, "a timed-out check fails open")
			assert.GreaterOrEqual(t, elapsed, ConsistencyCheckTimeout)
			assert.Less(t, elapsed, 5*time.Second, "the check must end at its own deadline")
			assert.Equal(t, 1, st.calls, "the check stops at the first timed-out lookup")
			assert.Equal(t, int64(1), DivergenceMetrics.ConsistencyTimeouts())
			assert.Equal(t, int64(0), DivergenceMetrics.ConsistencyChecks(), "a skipped check reaches no verdict")
			assert.Equal(t, int64(0), DivergenceMetrics.ConsistencyMismatches())
		})
	}
}

// A query error before the deadline fails open without counting a timeout.
func TestCheckConversationConsistency_CanceledCallerIsNotATimeout(t *testing.T) {
	swapDivergenceMetrics(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st := &stalledQueryStore{release: make(chan struct{})}
	assert.True(t, CheckConversationConsistency(ctx, st, "m1", "conv-1", "t1", "", "", quietLogger()))
	assert.Equal(t, int64(0), DivergenceMetrics.ConsistencyTimeouts())
}

// heldQueryStore holds every lookup until release is closed, ignoring the
// context, then returns items. A check can only see items through the
// release path, never through its deadline.
type heldQueryStore struct {
	entered chan struct{}
	release chan struct{}
	items   []store.Message
}

func (s *heldQueryStore) ListMessages(context.Context, store.MessageFilter, store.ListOptions) (*store.ListResult[store.Message], error) {
	s.entered <- struct{}{}
	<-s.release
	return &store.ListResult[store.Message]{Items: s.items}, nil
}

// Go returns while the check's lookup is still held, the check's context
// carries the runner's deadline, not the caller's, and the check's verdict
// comes from the released lookup.
func TestConsistencyRunner_GoDoesNotWaitForTheCheck(t *testing.T) {
	swapDivergenceMetrics(t)
	r := NewConsistencyRunner(2, time.Minute)
	st := &heldQueryStore{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
		// A prior message in another conversation: only the released lookup
		// can produce the mismatch verdict.
		items: []store.Message{{ID: "prior", ThreadID: "t1", ConversationID: "conv-other"}},
	}
	finished := make(chan bool, 1)
	var deadline time.Time
	var hasDeadline bool

	scheduled := r.Go(ConsistencyInput{
		Store: st, Log: quietLogger(), MessageID: "m1", ConversationID: "conv-1", ThreadID: "t1",
	}, func(ctx context.Context, in ConsistencyInput) {
		deadline, hasDeadline = ctx.Deadline()
		finished <- CheckConversationConsistency(ctx, in.Store, in.MessageID, in.ConversationID, in.ThreadID, in.SenderID, in.RecipientID, in.Log)
	})
	require.True(t, scheduled)

	select {
	case <-st.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the check never reached its lookup")
	}
	select {
	case <-finished:
		t.Fatal("the check finished while its lookup was held")
	default:
	}

	close(st.release)
	r.Wait()
	assert.False(t, <-finished, "the verdict comes from the released lookup")
	assert.Equal(t, int64(1), DivergenceMetrics.ConsistencyMismatches())
	assert.Equal(t, int64(0), DivergenceMetrics.ConsistencyTimeouts())
	require.True(t, hasDeadline, "the check runs on a context with a deadline")
	assert.WithinDuration(t, time.Now().Add(time.Minute), deadline, 30*time.Second)
}

// An input with nothing to check takes no slot, runs nothing and is not
// counted as a skip.
func TestConsistencyRunner_NothingToCheck(t *testing.T) {
	swapDivergenceMetrics(t)
	r := NewConsistencyRunner(1, time.Minute)
	ran := false
	fn := func(context.Context, ConsistencyInput) { ran = true }

	assert.False(t, r.Go(ConsistencyInput{SenderID: "a", RecipientID: "b"}, fn), "no conversation")
	assert.False(t, r.Go(ConsistencyInput{ConversationID: "c", SenderID: "a"}, fn), "neither a thread nor a pair")
	r.Wait()
	assert.False(t, ran)
	assert.Equal(t, int64(0), DivergenceMetrics.ConsistencySkipped())
}

// One check per lookup at a time: a second check for the same thread or
// pair (in either direction) and conversation is skipped and counted, while
// another pair still runs.
func TestConsistencyRunner_SkipsTheSameLookupInFlight(t *testing.T) {
	swapDivergenceMetrics(t)
	r := NewConsistencyRunner(4, time.Minute)
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	block := func(context.Context, ConsistencyInput) {
		started <- struct{}{}
		<-release
	}

	require.True(t, r.Go(ConsistencyInput{ConversationID: "c", SenderID: "a", RecipientID: "b"}, block))
	assert.False(t, r.Go(ConsistencyInput{ConversationID: "c", SenderID: "b", RecipientID: "a"}, block),
		"the reverse direction is the same lookup")
	require.True(t, r.Go(ConsistencyInput{ConversationID: "c", ThreadID: "t"}, block))
	assert.False(t, r.Go(ConsistencyInput{ConversationID: "c", ThreadID: "t"}, block))
	assert.True(t, r.Go(ConsistencyInput{ConversationID: "c", SenderID: "a", RecipientID: "x"}, block),
		"a different pair is a different lookup")
	assert.Equal(t, int64(2), DivergenceMetrics.ConsistencySkipped())

	close(release)
	r.Wait()
	assert.Len(t, started, 3)
	assert.True(t, r.Go(ConsistencyInput{ConversationID: "c", SenderID: "a", RecipientID: "b"}, func(context.Context, ConsistencyInput) {}),
		"the key is released when its check ends")
	r.Wait()
}

// When every slot is busy a new check is skipped and counted, not queued.
func TestConsistencyRunner_SkipsWhenEverySlotIsBusy(t *testing.T) {
	swapDivergenceMetrics(t)
	r := NewConsistencyRunner(1, time.Minute)
	release := make(chan struct{})
	ran := make(chan struct{}, 2)

	require.True(t, r.Go(ConsistencyInput{ConversationID: "c", SenderID: "a", RecipientID: "b"}, func(context.Context, ConsistencyInput) {
		ran <- struct{}{}
		<-release
	}))
	assert.False(t, r.Go(ConsistencyInput{ConversationID: "c", SenderID: "c", RecipientID: "d"}, func(context.Context, ConsistencyInput) {
		ran <- struct{}{}
	}))
	assert.Equal(t, int64(1), DivergenceMetrics.ConsistencySkipped())

	close(release)
	r.Wait()
	assert.Len(t, ran, 1, "the skipped check never runs")
}

// A panicking check releases its slot and key and does not crash the process.
func TestConsistencyRunner_ReleasesAfterPanic(t *testing.T) {
	swapDivergenceMetrics(t)
	r := NewConsistencyRunner(1, time.Minute)
	in := ConsistencyInput{Log: quietLogger(), ConversationID: "c", SenderID: "a", RecipientID: "b"}

	require.True(t, r.Go(in, func(context.Context, ConsistencyInput) { panic("boom") }))
	r.Wait()

	ran := false
	require.True(t, r.Go(in, func(context.Context, ConsistencyInput) { ran = true }),
		"the slot and the key are free again")
	r.Wait()
	assert.True(t, ran)
	assert.Equal(t, int64(0), DivergenceMetrics.ConsistencySkipped())
}

// A nil Log is defaulted before fn runs, so fn and the check never see nil.
func TestConsistencyRunner_DefaultsNilLog(t *testing.T) {
	swapDivergenceMetrics(t)
	r := NewConsistencyRunner(1, time.Minute)
	var got *slog.Logger
	require.True(t, r.Go(ConsistencyInput{ConversationID: "c", SenderID: "a", RecipientID: "b"},
		func(_ context.Context, in ConsistencyInput) { got = in.Log }))
	r.Wait()
	assert.NotNil(t, got)
}
