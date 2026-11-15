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

// Package durablequeue implements Scion's generic Postgres/SQLite-backed
// durable delivery queue and per-kind drainer.
package durablequeue

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Sentinel errors returned or recognized by durablequeue.
var (
	// ErrDuplicate is returned by Queue.Enqueue when an item with the same
	// (kind, dedup_key) already exists in the queue.
	ErrDuplicate = store.ErrDuplicate

	// ErrSinkBlocked may be returned in Result.Err by a Handler when its
	// downstream sink is temporarily unavailable across all items of the kind.
	// The drainer switches to one-item probes with exponential backoff without
	// counting blocked probes as item attempts.
	ErrSinkBlocked = errors.New("durablequeue: sink blocked")
)

// Item is one durable queue message.
type Item struct {
	Seq       int64
	Kind      string
	DedupKey  string    // "" -> Enqueue generates a UUID
	ProjectID string    // optional
	Payload   []byte    // opaque, immutable
	NotBefore time.Time // zero -> now
	Attempts  int
	CreatedAt time.Time
}

// Queue is the producer side. Enqueue runs inside the producer's transaction
// (tx is the store transaction handle the caller already holds) so a domain
// write and its queue item commit or roll back together. A duplicate
// (kind, dedup_key) returns ErrDuplicate and inserts nothing.
type Queue interface {
	Enqueue(ctx context.Context, tx store.Tx, item Item) (seq int64, err error)
	// Wake nudges the local drainer(s) of kind after commit; never blocks.
	Wake(kind string)
}

// QueueOption configures a StoreQueue.
type QueueOption func(*StoreQueue)

// WithQueueClock overrides the clock used by Enqueue when CreatedAt/NotBefore
// are zero. Intended for deterministic tests.
func WithQueueClock(now func() time.Time) QueueOption {
	return func(q *StoreQueue) {
		if now != nil {
			q.now = now
		}
	}
}

// drainerRegistrar is implemented by Queue implementations (such as StoreQueue)
// that can notify local drainers when Wake(kind) is called.
type drainerRegistrar interface {
	registerWake(kind string, fn func()) (unregister func())
}

// StoreQueue implements Queue over a store.DurableQueueStore and fans out
// Wake(kind) notifications to locally registered drainers of that kind.
type StoreQueue struct {
	store store.DurableQueueStore
	now   func() time.Time

	mu     sync.RWMutex
	nextID uint64
	wakers map[string]map[uint64]func()
}

var _ Queue = (*StoreQueue)(nil)
var _ drainerRegistrar = (*StoreQueue)(nil)

// NewQueue constructs a StoreQueue backed by s.
func NewQueue(s store.DurableQueueStore, opts ...QueueOption) *StoreQueue {
	q := &StoreQueue{
		store:  s,
		now:    time.Now,
		wakers: make(map[string]map[uint64]func()),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(q)
		}
	}
	return q
}

// Enqueue validates and inserts item into durable_queue_items within tx.
func (q *StoreQueue) Enqueue(ctx context.Context, tx store.Tx, item Item) (int64, error) {
	if q == nil || q.store == nil {
		return 0, errors.New("durablequeue: queue store is nil")
	}
	if err := store.ValidateDurableQueueKind(item.Kind); err != nil {
		return 0, err
	}
	dedupKey := item.DedupKey
	if dedupKey == "" {
		dedupKey = uuid.NewString()
	}
	now := q.now().UTC()
	createdAt := item.CreatedAt.UTC()
	if item.CreatedAt.IsZero() {
		createdAt = now
	}
	notBefore := item.NotBefore.UTC()
	if item.NotBefore.IsZero() {
		notBefore = createdAt
	}
	return q.store.EnqueueDurableItem(ctx, tx, store.DurableQueueRow{
		Kind:          item.Kind,
		DedupKey:      dedupKey,
		ProjectID:     item.ProjectID,
		Payload:       bytes.Clone(item.Payload),
		NotBefore:     notBefore,
		CreatedAt:     createdAt,
		NextAttemptAt: notBefore,
	})
}

// Wake nudges every local drainer registered for kind without blocking.
func (q *StoreQueue) Wake(kind string) {
	if q == nil {
		return
	}
	q.mu.RLock()
	byID := q.wakers[kind]
	fns := make([]func(), 0, len(byID))
	for _, fn := range byID {
		fns = append(fns, fn)
	}
	q.mu.RUnlock()

	for _, fn := range fns {
		fn()
	}
}

// Register attaches d to q so q.Wake(d.Kind()) nudges d. The returned function
// unregisters d and is safe to call multiple times.
func (q *StoreQueue) Register(d *Drainer) func() {
	if q == nil || d == nil {
		return func() {}
	}
	return q.registerWake(d.Kind(), d.Wake)
}

func (q *StoreQueue) registerWake(kind string, fn func()) func() {
	q.mu.Lock()
	q.nextID++
	id := q.nextID
	if q.wakers[kind] == nil {
		q.wakers[kind] = make(map[uint64]func())
	}
	q.wakers[kind][id] = fn
	q.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			q.mu.Lock()
			if m := q.wakers[kind]; m != nil {
				delete(m, id)
				if len(m) == 0 {
					delete(q.wakers, kind)
				}
			}
			q.mu.Unlock()
		})
	}
}

func rowToItem(r store.DurableQueueRow) Item {
	return Item{
		Seq:       r.Seq,
		Kind:      r.Kind,
		DedupKey:  r.DedupKey,
		ProjectID: r.ProjectID,
		Payload:   bytes.Clone(r.Payload),
		NotBefore: r.NotBefore,
		Attempts:  r.Attempts,
		CreatedAt: r.CreatedAt,
	}
}
