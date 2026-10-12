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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readStateBatchRecorder records the key count of each GetReadStates call.
type readStateBatchRecorder struct {
	WebChatStore
	calls []int
}

func (r *readStateBatchRecorder) GetReadStates(_ context.Context, _ string, keys []string) ([]WebChatReadState, error) {
	r.calls = append(r.calls, len(keys))
	out := make([]WebChatReadState, len(keys))
	for i, k := range keys {
		out[i] = WebChatReadState{ConversationKey: k}
	}
	return out, nil
}

// A batch of zero or less falls back to the default size instead of
// looping without advancing.
func TestChatUnreadCount_ReadStatesBatchNonPositive(t *testing.T) {
	keys := []string{"a", "b", "c"}
	for _, batch := range []int{-1, 0} {
		rec := &readStateBatchRecorder{}
		got, err := chatReadStatesBatched(context.Background(), rec, "u1", keys, batch)
		require.NoError(t, err)
		assert.Len(t, got, len(keys), "batch %d", batch)
		assert.Equal(t, []int{len(keys)}, rec.calls, "batch %d", batch)
	}

	rec := &readStateBatchRecorder{}
	_, err := chatReadStatesBatched(context.Background(), rec, "u1", keys, 2)
	require.NoError(t, err)
	assert.Equal(t, []int{2, 1}, rec.calls)
}
