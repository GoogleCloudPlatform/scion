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

package eventfeed

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStore is an in-memory Store for unit testing Feed without pkg/hub or a DB.
type fakeStore struct {
	mu      sync.Mutex
	heads   map[string]int64
	msgs    map[string][]store.Message
	deleted map[string]map[int64]bool
	onList  func(call int) // optional hook invoked before ListConversationFeed executes
	lists   int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		heads:   make(map[string]int64),
		msgs:    make(map[string][]store.Message),
		deleted: make(map[string]map[int64]bool),
	}
}

func (s *fakeStore) append(convID string, m store.Message) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heads[convID]++
	seq := s.heads[convID]
	m.ConversationID = convID
	m.ConversationSeq = seq
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	s.msgs[convID] = append(s.msgs[convID], m)
	return seq
}

func (s *fakeStore) deleteSeq(convID string, seq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleted[convID] == nil {
		s.deleted[convID] = make(map[int64]bool)
	}
	s.deleted[convID][seq] = true
}

func (s *fakeStore) ConversationHeadSeq(_ context.Context, conversationID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heads[conversationID], nil
}

func (s *fakeStore) MessageConversationSeq(_ context.Context, conversationID, messageID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.msgs[conversationID] {
		if m.ID == messageID && !s.deleted[conversationID][m.ConversationSeq] {
			return m.ConversationSeq, nil
		}
	}
	return 0, store.ErrNotFound
}

func (s *fakeStore) ListConversationFeed(_ context.Context, q store.ConversationFeedQuery) (store.ConversationFeedPage, error) {
	s.mu.Lock()
	s.lists++
	call := s.lists
	hook := s.onList
	s.mu.Unlock()

	if hook != nil {
		hook(call)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	head := s.heads[q.ConversationID]
	if head <= q.AfterSeq {
		return store.ConversationFeedPage{Messages: []store.Message{}, ScannedTo: q.AfterSeq}, nil
	}

	// Gap detection matching entadapter.ListConversationFeed.
	var truncated bool
	if q.AfterSeq > 0 {
		var minSeq int64
		for _, m := range s.msgs[q.ConversationID] {
			if s.deleted[q.ConversationID][m.ConversationSeq] {
				continue
			}
			if m.ConversationSeq > q.AfterSeq && m.ConversationSeq <= head {
				if minSeq == 0 || m.ConversationSeq < minSeq {
					minSeq = m.ConversationSeq
				}
			}
		}
		if minSeq == 0 || minSeq > q.AfterSeq+1 {
			truncated = true
		}
	}

	var matched []store.Message
	for _, m := range s.msgs[q.ConversationID] {
		if s.deleted[q.ConversationID][m.ConversationSeq] {
			continue
		}
		if m.ConversationSeq <= q.AfterSeq || m.ConversationSeq > head {
			continue
		}
		isSelf := q.PrincipalKind != "" && q.PrincipalID != "" &&
			m.SenderID == q.PrincipalID && strings.HasPrefix(m.Sender, q.PrincipalKind+":")
		switch q.Filter {
		case store.ConversationFeedFilterOthers, "":
			if isSelf {
				continue
			}
		case store.ConversationFeedFilterAddressed:
			if isSelf {
				continue
			}
			if m.RecipientID != q.PrincipalID || !strings.HasPrefix(m.Recipient, q.PrincipalKind+":") {
				continue
			}
		case store.ConversationFeedFilterAll:
		}
		matched = append(matched, m)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	hasMore := len(matched) > limit
	if hasMore {
		matched = matched[:limit]
	}
	scannedTo := head
	if hasMore && len(matched) > 0 {
		scannedTo = matched[len(matched)-1].ConversationSeq
	}
	return store.ConversationFeedPage{
		Messages:  matched,
		ScannedTo: scannedTo,
		HasMore:   hasMore,
		Truncated: truncated,
	}, nil
}

// fakeWaker is an in-memory coalescing Waker for unit tests.
type fakeWaker struct {
	mu          sync.Mutex
	subs        map[string]map[chan struct{}]struct{}
	onSubscribe func(convID string)
}

func newFakeWaker() *fakeWaker {
	return &fakeWaker{subs: make(map[string]map[chan struct{}]struct{})}
}

func (w *fakeWaker) Subscribe(conversationID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	if w.subs[conversationID] == nil {
		w.subs[conversationID] = make(map[chan struct{}]struct{})
	}
	w.subs[conversationID][ch] = struct{}{}
	hook := w.onSubscribe
	w.mu.Unlock()

	if hook != nil {
		hook(conversationID)
	}

	cancel := func() {
		w.mu.Lock()
		delete(w.subs[conversationID], ch)
		w.mu.Unlock()
	}
	return ch, cancel
}

func (w *fakeWaker) Wake(conversationID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for ch := range w.subs[conversationID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func allowAuthz(_ context.Context, convID string) (*store.Conversation, error) {
	return &store.Conversation{ID: convID}, nil
}

// Criterion 1: Poll returns immediately when events exist; otherwise blocks
// until wakeup and returns that event.
func TestPoll_ImmediateAndWakeup(t *testing.T) {
	st := newFakeStore()
	wk := newFakeWaker()
	feed := NewFeed(st, wk)
	feed.RecheckInterval = 10 * time.Second

	convID := "conv-1"
	st.append(convID, store.Message{ID: "m1", Sender: "agent:a", SenderID: "a1", Msg: "first"})

	// Immediate return when events exist from beginning.
	res, err := feed.Poll(context.Background(), Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
		ConversationID: convID,
		Start:          StartBeginning,
		Wait:           5 * time.Second,
	})
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	assert.Equal(t, "msg:m1", res.Events[0].EventID)
	assert.Equal(t, TypeConversationMessageCreated, res.Events[0].Type)
	assert.Equal(t, "scion://conversations/conv-1/messages/m1", res.Events[0].ResourceURI)
	assert.False(t, res.TimedOut)

	// Block until wakeup when no newer events exist.
	done := make(chan PollResult, 1)
	errCh := make(chan error, 1)
	go func() {
		r, e := feed.Poll(context.Background(), Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
			ConversationID: convID,
			Cursor:         res.NextCursor,
			Wait:           5 * time.Second,
		})
		if e != nil {
			errCh <- e
			return
		}
		done <- r
	}()

	// Wait briefly until the goroutine is subscribed, then write & wake.
	time.Sleep(25 * time.Millisecond)
	st.append(convID, store.Message{ID: "m2", Sender: "agent:a", SenderID: "a1", Msg: "second"})
	wk.Wake(convID)

	select {
	case err := <-errCh:
		t.Fatalf("unexpected error: %v", err)
	case r := <-done:
		require.Len(t, r.Events, 1)
		assert.Equal(t, "msg:m2", r.Events[0].EventID)
		assert.False(t, r.TimedOut)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for wakeup delivery")
	}
}

// Criterion 2: Subscribe/requery race — an event written between the first
// query and the subscribe is returned immediately without waiting for timeout.
func TestPoll_SubscribeRequeryRace(t *testing.T) {
	st := newFakeStore()
	wk := newFakeWaker()
	feed := NewFeed(st, wk)
	feed.RecheckInterval = 10 * time.Second

	convID := "conv-race"
	// Inject a write right as Subscribe is called (after first query returned
	// empty, without calling wk.Wake).
	wk.onSubscribe = func(id string) {
		st.append(id, store.Message{ID: "m-race", Sender: "agent:a", SenderID: "a1", Msg: "raced"})
	}

	start := time.Now()
	res, err := feed.Poll(context.Background(), Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
		ConversationID: convID,
		Start:          StartHead,
		Wait:           5 * time.Second,
	})
	require.NoError(t, err)
	assert.Less(t, time.Since(start), time.Second, "must return from step-4 requery, not wait for timeout")
	require.Len(t, res.Events, 1)
	assert.Equal(t, "msg:m-race", res.Events[0].EventID)
}

// Criterion 3: Missed wakeup is recovered by the recheck tick within RecheckInterval.
func TestPoll_MissedWakeupRecoveredByRecheck(t *testing.T) {
	st := newFakeStore()
	wk := newFakeWaker()
	feed := NewFeed(st, wk)
	feed.RecheckInterval = 40 * time.Millisecond

	convID := "conv-recheck"
	go func() {
		time.Sleep(20 * time.Millisecond)
		// Write without calling wk.Wake (simulating a dropped NOTIFY).
		st.append(convID, store.Message{ID: "m-missed", Sender: "agent:a", SenderID: "a1", Msg: "recovered"})
	}()

	start := time.Now()
	res, err := feed.Poll(context.Background(), Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
		ConversationID: convID,
		Start:          StartHead,
		Wait:           2 * time.Second,
	})
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 500*time.Millisecond)
	require.Len(t, res.Events, 1)
	assert.Equal(t, "msg:m-missed", res.Events[0].EventID)
}

// Criterion 4: Timeout returns TimedOut, empty events, and non-empty NextCursor
// equal to head; a follow-up poll returns only newer events.
func TestPoll_TimeoutAndFollowUp(t *testing.T) {
	st := newFakeStore()
	wk := newFakeWaker()
	feed := NewFeed(st, wk)
	feed.RecheckInterval = 20 * time.Millisecond

	convID := "conv-timeout"
	st.append(convID, store.Message{ID: "m1", Sender: "agent:a", SenderID: "a1", Msg: "old"})

	res, err := feed.Poll(context.Background(), Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
		ConversationID: convID,
		Start:          StartHead,
		Wait:           50 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.True(t, res.TimedOut)
	assert.Empty(t, res.Events)
	assert.Equal(t, EncodeCursor(convID, 1), res.NextCursor)

	st.append(convID, store.Message{ID: "m2", Sender: "agent:a", SenderID: "a1", Msg: "new"})

	res2, err := feed.Poll(context.Background(), Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
		ConversationID: convID,
		Cursor:         res.NextCursor,
		Wait:           0,
	})
	require.NoError(t, err)
	assert.False(t, res2.TimedOut)
	require.Len(t, res2.Events, 1)
	assert.Equal(t, "msg:m2", res2.Events[0].EventID)
}

// Criterion 5: Filtered-out rows (own sends under others) advance NextCursor
// and do not end the wait early.
func TestPoll_FilteredOutRowsAdvanceCursorAndKeepWaiting(t *testing.T) {
	st := newFakeStore()
	wk := newFakeWaker()
	feed := NewFeed(st, wk)
	feed.RecheckInterval = 20 * time.Millisecond

	convID := "conv-filter"
	go func() {
		time.Sleep(15 * time.Millisecond)
		// Caller's own send — should be filtered out under FilterOthers.
		st.append(convID, store.Message{ID: "m-self", Sender: "user:alice", SenderID: "u1", Msg: "my own msg"})
		wk.Wake(convID)
	}()

	start := time.Now()
	res, err := feed.Poll(context.Background(), Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
		ConversationID: convID,
		Start:          StartHead,
		Filter:         FilterOthers,
		Wait:           80 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, time.Since(start), 70*time.Millisecond, "own send must not end wait early")
	assert.True(t, res.TimedOut)
	assert.Empty(t, res.Events)
	// NextCursor advanced past the filtered-out seq 1.
	assert.Equal(t, EncodeCursor(convID, 1), res.NextCursor)
}

// Criterion 6: Cursor errors — bad encoding/version -> ErrInvalidCursor,
// wrong conversation -> ErrInvalidCursor, seq > head -> ErrCursorExpired.
func TestPoll_CursorErrors(t *testing.T) {
	st := newFakeStore()
	feed := NewFeed(st, nil)
	convID := "conv-a"
	st.append(convID, store.Message{ID: "m1", Sender: "agent:a", SenderID: "a1"})

	// Bad base64 encoding.
	_, err := feed.Poll(context.Background(), Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
		ConversationID: convID,
		Cursor:         Cursor("!!!not-base64!!!"),
	})
	assert.ErrorIs(t, err, ErrInvalidCursor)

	// Wrong version.
	badVer := Cursor(base64.RawURLEncoding.EncodeToString([]byte("v2." + convID + ".1")))
	_, err = feed.Poll(context.Background(), Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
		ConversationID: convID,
		Cursor:         badVer,
	})
	assert.ErrorIs(t, err, ErrInvalidCursor)

	// Bound to another conversation.
	otherCursor := EncodeCursor("conv-b", 1)
	_, err = feed.Poll(context.Background(), Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
		ConversationID: convID,
		Cursor:         otherCursor,
	})
	assert.ErrorIs(t, err, ErrInvalidCursor)

	// Future seq > head -> ErrCursorExpired.
	futureCursor := EncodeCursor(convID, 99)
	_, err = feed.Poll(context.Background(), Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
		ConversationID: convID,
		Cursor:         futureCursor,
	})
	assert.ErrorIs(t, err, ErrCursorExpired)

	// Missing AfterMessageID -> ErrInvalidCursor.
	_, err = feed.Poll(context.Background(), Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
		ConversationID: convID,
		AfterMessageID: "missing-id",
	})
	assert.ErrorIs(t, err, ErrInvalidCursor)
}

// Criterion 7: Truncated is set when rows between the cursor and the first
// event were deleted.
func TestPoll_TruncatedOnDeletedRows(t *testing.T) {
	st := newFakeStore()
	feed := NewFeed(st, nil)
	convID := "conv-trunc"
	st.append(convID, store.Message{ID: "m1", Sender: "agent:a", SenderID: "a1"})
	st.append(convID, store.Message{ID: "m2", Sender: "agent:a", SenderID: "a1"})
	st.append(convID, store.Message{ID: "m3", Sender: "agent:a", SenderID: "a1"})
	st.deleteSeq(convID, 2)

	res, err := feed.Poll(context.Background(), Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
		ConversationID: convID,
		Cursor:         EncodeCursor(convID, 1),
		Wait:           0,
	})
	require.NoError(t, err)
	assert.True(t, res.Truncated)
	require.Len(t, res.Events, 1)
	assert.Equal(t, "msg:m3", res.Events[0].EventID)
}

// Criterion 8: Watch advances the cursor only after emit succeeds, and stops on
// ctx cancel.
func TestWatch_EmitErrorAndCancel(t *testing.T) {
	st := newFakeStore()
	wk := newFakeWaker()
	feed := NewFeed(st, wk)
	feed.RecheckInterval = 20 * time.Millisecond

	convID := "conv-watch"
	st.append(convID, store.Message{ID: "m1", Sender: "agent:a", SenderID: "a1"})
	st.append(convID, store.Message{ID: "m2", Sender: "agent:a", SenderID: "a1"})

	// Fail on second event: cursor must not advance past m1.
	emitBoom := errors.New("sink failed")
	var seen []string
	var lastOK Cursor
	err := feed.Watch(context.Background(), Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
		ConversationID: convID,
		Start:          StartBeginning,
		Wait:           50 * time.Millisecond,
	}, func(e Event) error {
		seen = append(seen, e.EventID)
		if e.EventID == "msg:m2" {
			return emitBoom
		}
		lastOK = e.Cursor
		return nil
	})
	assert.ErrorIs(t, err, emitBoom)
	assert.Equal(t, []string{"msg:m1", "msg:m2"}, seen)
	assert.Equal(t, EncodeCursor(convID, 1), lastOK)

	// Resume from lastOK: m2 is redelivered, then cancel stops Watch cleanly.
	ctx, cancel := context.WithCancel(context.Background())
	var resumed []string
	err = feed.Watch(ctx, Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
		ConversationID: convID,
		Cursor:         lastOK,
		Wait:           200 * time.Millisecond,
	}, func(e Event) error {
		resumed = append(resumed, e.EventID)
		cancel()
		return nil
	})
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []string{"msg:m2"}, resumed)
}

type closedWaker struct{}

func (closedWaker) Subscribe(_ string) (<-chan struct{}, func()) {
	ch := make(chan struct{})
	close(ch)
	return ch, func() {}
}

// Regression test: if a Waker implementation closes its channel, Poll must
// nil out wakeCh so select does not busy-spin, and still recover via recheck
// or return cleanly at deadline.
func TestPoll_ClosedWakeChannelDoesNotSpin(t *testing.T) {
	st := newFakeStore()
	feed := NewFeed(st, closedWaker{})
	feed.RecheckInterval = 30 * time.Millisecond

	convID := "conv-closed-wake"
	res, err := feed.Poll(context.Background(), Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
		ConversationID: convID,
		Start:          StartHead,
		Wait:           70 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.True(t, res.TimedOut)
	assert.Empty(t, res.Events)
	// Without the !ok -> wakeCh=nil guard, ListConversationFeed would be called
	// thousands of times in 70ms; with it, only initial + step-4 + <=3 rechecks.
	st.mu.Lock()
	calls := st.lists
	st.mu.Unlock()
	assert.LessOrEqual(t, calls, 6, "Poll must not busy-spin on a closed wakeCh")
}

// Phase 3: StopCh ends a blocking wait early with empty events, TimedOut=false,
// and the current NextCursor (while client ctx cancellation still returns ctx.Err()).
func TestPoll_StopChEarlyReturn(t *testing.T) {
	st := newFakeStore()
	wk := newFakeWaker()
	feed := NewFeed(st, wk)
	feed.RecheckInterval = 5 * time.Second

	convID := "conv-stopch"
	st.append(convID, store.Message{ID: "m1", Sender: "agent:a1", SenderID: "a1", Msg: "seed"})

	// 1. Closing StopCh mid-wait returns early with TimedOut=false and current cursor.
	stopCh := make(chan struct{})
	go func() {
		time.Sleep(30 * time.Millisecond)
		close(stopCh)
	}()

	start := time.Now()
	res, err := feed.Poll(context.Background(), Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
		ConversationID: convID,
		Start:          StartHead,
		Wait:           5 * time.Second,
		StopCh:         stopCh,
		StopRetryAfter: time.Second,
	})
	require.NoError(t, err)
	assert.Less(t, time.Since(start), time.Second)
	assert.Empty(t, res.Events)
	assert.False(t, res.TimedOut)
	assert.Equal(t, time.Second, res.RetryAfter)
	assert.Equal(t, EncodeCursor(convID, 1), res.NextCursor)

	// 2. Pre-closed StopCh returns immediately without waiting.
	preClosed := make(chan struct{})
	close(preClosed)
	start = time.Now()
	res2, err := feed.Poll(context.Background(), Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
		ConversationID: convID,
		Start:          StartHead,
		Wait:           5 * time.Second,
		StopCh:         preClosed,
		StopRetryAfter: time.Second,
	})
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 200*time.Millisecond)
	assert.Empty(t, res2.Events)
	assert.False(t, res2.TimedOut)
	assert.Equal(t, time.Second, res2.RetryAfter)
	assert.Equal(t, EncodeCursor(convID, 1), res2.NextCursor)

	// 3. When ctx is also canceled, ctx.Err() takes precedence over StopCh.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = feed.Poll(ctx, Principal{Kind: "user", ID: "u1"}, allowAuthz, PollRequest{
		ConversationID: convID,
		Start:          StartHead,
		Wait:           5 * time.Second,
		StopCh:         preClosed,
	})
	assert.ErrorIs(t, err, context.Canceled)
}
