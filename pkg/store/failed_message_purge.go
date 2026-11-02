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

package store

import "time"

// MessagePosition is a keyset position in messages ordered by (created, id).
// The zero value is the start of the walk.
type MessagePosition struct {
	Created time.Time
	ID      string
}

// IsStart reports whether p is the start of the walk.
func (p MessagePosition) IsStart() bool { return p.Created.IsZero() && p.ID == "" }

// FailedMessagePurgeChunk is the result of one PurgeFailedMessagesChunk call.
type FailedMessagePurgeChunk struct {
	// Deleted is the number of messages the chunk removed.
	Deleted int
	// Examined is the number of candidate rows the chunk walked past,
	// including failed rows it keeps (non-agent recipients).
	Examined int
	// Next is the position to pass as after on the next call. It equals
	// the after argument when the chunk examined no rows.
	Next MessagePosition
	// Done reports that the walk reached the last candidate before the
	// cutoff: the next pass starts again from the beginning.
	Done bool
}
