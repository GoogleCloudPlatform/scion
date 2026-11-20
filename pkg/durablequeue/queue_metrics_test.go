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

package durablequeue_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
)

type spyQueueMetrics struct {
	mu       sync.Mutex
	enqueued map[string]int
}

func (s *spyQueueMetrics) Enqueued(kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enqueued == nil {
		s.enqueued = make(map[string]int)
	}
	s.enqueued[kind]++
}

func TestStoreQueue_WithQueueMetrics(t *testing.T) {
	ctx := context.Background()
	st := newMemQueueStore()
	m := &spyQueueMetrics{}
	q := durablequeue.NewQueue(st, durablequeue.WithQueueMetrics(m))

	_, err := q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:     "test.echo",
		DedupKey: "k-1",
		Payload:  []byte(`{"ok":true}`),
	})
	require.NoError(t, err)

	// Duplicate enqueue must not increment Enqueued.
	_, err = q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:     "test.echo",
		DedupKey: "k-1",
		Payload:  []byte(`{"ok":true}`),
	})
	require.ErrorIs(t, err, durablequeue.ErrDuplicate)

	assert.Equal(t, 1, m.enqueued["test.echo"])
}
