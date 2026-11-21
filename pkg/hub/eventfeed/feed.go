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
	"errors"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// DefaultLimit is the default maximum number of events returned per Poll.
	DefaultLimit = 50
	// MaxLimit is the upper bound on Limit per Poll.
	MaxLimit = 200
	// DefaultMaxWait is the default upper bound on server-side long-poll wait.
	DefaultMaxWait = 55 * time.Second
	// DefaultRecheckInterval is the fallback DB recheck cadence while waiting
	// for a wakeup notification.
	DefaultRecheckInterval = 5 * time.Second
)

// Feed serves stateless long-poll and streaming reads over conversation feeds.
type Feed struct {
	store           Store
	waker           Waker
	MaxWait         time.Duration
	RecheckInterval time.Duration
}

// NewFeed constructs a Feed with the given store and waker. waker may be nil
// (in which case Poll relies solely on RecheckInterval).
func NewFeed(s Store, w Waker) *Feed {
	return &Feed{
		store:           s,
		waker:           w,
		MaxWait:         DefaultMaxWait,
		RecheckInterval: DefaultRecheckInterval,
	}
}

func (f *Feed) maxWait() time.Duration {
	if f.MaxWait <= 0 {
		return DefaultMaxWait
	}
	return f.MaxWait
}

func (f *Feed) recheckInterval() time.Duration {
	if f.RecheckInterval <= 0 {
		return DefaultRecheckInterval
	}
	return f.RecheckInterval
}

// Poll performs one race-free, stateless long-poll read over a conversation.
func (f *Feed) Poll(ctx context.Context, p Principal, authz Authorizer, req PollRequest) (PollResult, error) {
	if authz == nil {
		return PollResult{}, ErrForbidden
	}
	if _, err := authz(ctx, req.ConversationID); err != nil {
		return PollResult{}, err
	}

	filter, err := normalizeFilter(req.Filter)
	if err != nil {
		return PollResult{}, err
	}
	start, err := normalizeStart(req.Start)
	if err != nil {
		return PollResult{}, err
	}
	limit := normalizeLimit(req.Limit)
	wait := f.clampWait(req.Wait)

	head, err := f.store.ConversationHeadSeq(ctx, req.ConversationID)
	if err != nil {
		return PollResult{}, err
	}

	after, err := f.resolveAfterSeq(ctx, req, start, head)
	if err != nil {
		return PollResult{}, err
	}

	// Step 3: initial query before subscribing.
	res, scannedTo, err := f.fetch(ctx, req.ConversationID, after, limit, filter, p)
	if err != nil {
		return PollResult{}, err
	}
	if len(res.Events) > 0 || wait == 0 {
		return res, nil
	}
	truncated := res.Truncated
	after = scannedTo

	// Step 4: subscribe to waker BEFORE re-checking to close the race window.
	var (
		wakeCh <-chan struct{}
		cancel func()
	)
	if f.waker != nil {
		wakeCh, cancel = f.waker.Subscribe(req.ConversationID)
		defer cancel()
	}

	res, scannedTo, err = f.fetch(ctx, req.ConversationID, after, limit, filter, p)
	if err != nil {
		return PollResult{}, err
	}
	truncated = truncated || res.Truncated
	if len(res.Events) > 0 {
		res.Truncated = truncated
		return res, nil
	}
	after = scannedTo

	// Step 5: wait for wakeup, recheck tick, deadline, or context cancellation.
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	recheck := time.NewTicker(f.recheckInterval())
	defer recheck.Stop()

	for {
		select {
		case <-ctx.Done():
			return PollResult{}, ctx.Err()
		case <-deadline.C:
			return PollResult{
				Events:     []Event{},
				NextCursor: EncodeCursor(req.ConversationID, after),
				Truncated:  truncated,
				TimedOut:   true,
			}, nil
		case _, ok := <-wakeCh:
			if !ok {
				wakeCh = nil
				continue
			}
		case <-recheck.C:
		}

		res, scannedTo, err = f.fetch(ctx, req.ConversationID, after, limit, filter, p)
		if err != nil {
			return PollResult{}, err
		}
		truncated = truncated || res.Truncated
		after = scannedTo
		if len(res.Events) > 0 {
			res.Truncated = truncated
			return res, nil
		}
	}
}

// Watch is the transport-neutral streaming primitive built as a loop over Poll.
// It calls emit for each event in order and advances its cursor only after emit
// returns nil. Watch stops when ctx is cancelled or emit/Poll returns an error.
func (f *Feed) Watch(ctx context.Context, p Principal, authz Authorizer, req PollRequest, emit func(Event) error) error {
	if emit == nil {
		return fmt.Errorf("eventfeed: emit callback is required")
	}
	cur := req
	if cur.Wait <= 0 {
		cur.Wait = f.maxWait()
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := f.Poll(ctx, p, authz, cur)
		if err != nil {
			return err
		}
		for _, evt := range res.Events {
			if err := emit(evt); err != nil {
				return err
			}
			cur.Cursor = evt.Cursor
			cur.AfterMessageID = ""
			cur.Start = ""
		}
		if res.NextCursor != "" {
			cur.Cursor = res.NextCursor
			cur.AfterMessageID = ""
			cur.Start = ""
		}
	}
}

func (f *Feed) resolveAfterSeq(ctx context.Context, req PollRequest, start Start, head int64) (int64, error) {
	if req.Cursor != "" {
		seq, err := DecodeCursor(req.Cursor, req.ConversationID)
		if err != nil {
			return 0, err
		}
		if seq > head {
			return 0, ErrCursorExpired
		}
		return seq, nil
	}
	if req.AfterMessageID != "" {
		seq, err := f.store.MessageConversationSeq(ctx, req.ConversationID, req.AfterMessageID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
				return 0, ErrInvalidCursor
			}
			return 0, err
		}
		return seq, nil
	}
	if start == StartBeginning {
		return 0, nil
	}
	return head, nil
}

func (f *Feed) fetch(
	ctx context.Context,
	convID string,
	after int64,
	limit int,
	filter store.ConversationFeedFilter,
	p Principal,
) (PollResult, int64, error) {
	page, err := f.store.ListConversationFeed(ctx, store.ConversationFeedQuery{
		ConversationID: convID,
		AfterSeq:       after,
		Limit:          limit,
		Filter:         filter,
		PrincipalKind:  p.Kind,
		PrincipalID:    p.ID,
	})
	if err != nil {
		return PollResult{}, after, err
	}

	events := make([]Event, 0, len(page.Messages))
	for i := range page.Messages {
		m := page.Messages[i]
		events = append(events, Event{
			EventID:     "msg:" + m.ID,
			Type:        TypeConversationMessageCreated,
			OccurredAt:  m.CreatedAt.UTC(),
			Cursor:      EncodeCursor(convID, m.ConversationSeq),
			ResourceURI: fmt.Sprintf("scion://conversations/%s/messages/%s", convID, m.ID),
			Data:        &m,
		})
	}

	scannedTo := page.ScannedTo
	if scannedTo < after {
		scannedTo = after
	}

	return PollResult{
		Events:     events,
		NextCursor: EncodeCursor(convID, scannedTo),
		HasMore:    page.HasMore,
		Truncated:  page.Truncated,
	}, scannedTo, nil
}

func normalizeFilter(f Filter) (store.ConversationFeedFilter, error) {
	switch f {
	case "", FilterOthers:
		return store.ConversationFeedFilterOthers, nil
	case FilterAll:
		return store.ConversationFeedFilterAll, nil
	case FilterAddressed:
		return store.ConversationFeedFilterAddressed, nil
	default:
		return "", fmt.Errorf("%w: invalid filter %q", store.ErrInvalidInput, f)
	}
}

func normalizeStart(s Start) (Start, error) {
	switch s {
	case "", StartHead:
		return StartHead, nil
	case StartBeginning:
		return StartBeginning, nil
	default:
		return "", fmt.Errorf("%w: invalid start %q", store.ErrInvalidInput, s)
	}
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}

func (f *Feed) clampWait(w time.Duration) time.Duration {
	if w <= 0 {
		return 0
	}
	maxW := f.maxWait()
	if w > maxW {
		return maxW
	}
	return w
}
