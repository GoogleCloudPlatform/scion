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

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AgentCursor is the decoded form of a v2 sorted-mode agent list cursor:
// the position key K, the tie-break Created timestamp, and the id of the
// last examined item.
type AgentCursor struct {
	K       time.Time
	Created time.Time
	ID      string
}

// agentCursorV2Prefix marks a sorted-mode cursor. A legacy (pre-sort) cursor
// never starts with it, so a legacy cursor replayed in sorted mode, or a v2
// cursor replayed in legacy mode, both fail to decode rather than silently
// misparsing.
const agentCursorV2Prefix = "v2"

// EncodeAgentCursor produces the opaque v2 cursor for a sorted-mode agent
// list page:
//
//	base64url( "v2," sort "," dir "," RFC3339Nano(K) "," RFC3339Nano(created) "," uuid "," binding )
//
// sort and dir are embedded in the payload (not just the binding) so that a
// cursor minted for one sort or direction is rejected, with the same 400,
// when replayed against another — DecodeAgentCursor checks both before
// touching the binding.
func EncodeAgentCursor(sort, dir string, k, created time.Time, id, binding string) string {
	raw := strings.Join([]string{
		agentCursorV2Prefix,
		sort,
		dir,
		k.UTC().Format(time.RFC3339Nano),
		created.UTC().Format(time.RFC3339Nano),
		id,
		binding,
	}, ",")
	return base64.URLEncoding.EncodeToString([]byte(raw))
}

// DecodeAgentCursor decodes and validates a v2 sorted-mode cursor against the
// request's sort, dir and binding, before any store call is made. Every
// failure — malformed input, a legacy cursor, a mismatched sort, dir or
// binding, or an unparseable timestamp or id — is reported by wrapping
// ErrInvalidInput, so callers can map it to a uniform 400 the same way they
// already do for store.ErrInvalidInput.
func DecodeAgentCursor(cursor, sort, dir, binding string) (AgentCursor, error) {
	raw, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return AgentCursor{}, fmt.Errorf("invalid cursor: %w", ErrInvalidInput)
	}
	// binding is itself an opaque token that may contain commas (it is a
	// hash in practice, but nothing here relies on that), so split into
	// exactly 7 fields — v2, sort, dir, K, created, id, binding — with the
	// 7th capturing everything from the id's trailing comma on, unsplit.
	parts := strings.SplitN(string(raw), ",", 7)
	if len(parts) != 7 {
		return AgentCursor{}, fmt.Errorf("invalid cursor: %w", ErrInvalidInput)
	}
	if parts[0] != agentCursorV2Prefix {
		return AgentCursor{}, fmt.Errorf("invalid cursor: not a sorted-mode cursor: %w", ErrInvalidInput)
	}
	if parts[1] != sort || parts[2] != dir {
		return AgentCursor{}, fmt.Errorf("invalid cursor: sort/dir mismatch: %w", ErrInvalidInput)
	}
	if parts[6] != binding {
		return AgentCursor{}, fmt.Errorf("invalid cursor: %w", ErrInvalidInput)
	}
	k, err := time.Parse(time.RFC3339Nano, parts[3])
	if err != nil {
		return AgentCursor{}, fmt.Errorf("invalid cursor: parse key: %w", ErrInvalidInput)
	}
	created, err := time.Parse(time.RFC3339Nano, parts[4])
	if err != nil {
		return AgentCursor{}, fmt.Errorf("invalid cursor: parse created: %w", ErrInvalidInput)
	}
	// uuid.Parse also accepts urn:uuid:, braced, undashed and uppercase
	// forms. Agent ids are always stored in the canonical lowercase dashed
	// form, so anything else is rejected rather than bound into SQL.
	parsed, err := uuid.Parse(parts[5])
	if err != nil || parsed.String() != parts[5] {
		return AgentCursor{}, fmt.Errorf("invalid cursor: parse id: %w", ErrInvalidInput)
	}
	return AgentCursor{K: k, Created: created, ID: parts[5]}, nil
}
