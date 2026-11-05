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

package tmuxdeliver

import (
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

// allowedKeys is the key allowlist (design v2.22 §5.3): the only tmux key
// names a delivery over the deliver-message route may send. It is deny by
// default and matched exactly: no other key name, no literal string, and
// nothing that send-keys would read as a flag (a leading '-') can pass.
//
// It holds exactly the keys the shipped harnesses use (Enter, Escape, C-c);
// TestShippedHarnessesFitRouteBounds shows that is enough. Keys such as C-d
// (end of input, which can exit a harness at an empty prompt) are left out on
// purpose, so a delivery can never stop an agent. Adding a key needs a
// harness that uses it, a test and a security review.
var allowedKeys = map[string]struct{}{
	"Enter":  {},
	"Escape": {},
	"C-c":    {},
}

// Route bounds (design v2.22 §1.7). They are narrower than what harness
// config allows. A delivery whose settings fall outside them is not
// delivered over the deliver-message route: the broker uses exec where the
// runtime has it, or reports a failure naming the field. No value is ever
// clamped.
const (
	MaxKeys             = 4
	MaxExtraSubmits     = 3
	MaxExtraSubmitDelay = 2 * time.Second
	MaxReadySettle      = 10 * time.Second
	// MaxReadyTimeout is the route's readiness bound. The route's 92s hard
	// stop is set against it, so it is an explicit value, not the shared
	// readiness default.
	MaxReadyTimeout = 90 * time.Second
)

// Field names a BoundsError reports. They name settings, never values.
const (
	FieldSubmitKeys       = "submit_keys"
	FieldInterruptKeys    = "interrupt_keys"
	FieldExtraSubmits     = "extra_submits"
	FieldExtraSubmitDelay = "extra_submit_delay"
	FieldReadyStrategy    = "ready.strategy"
	FieldReadySettle      = "ready.settle"
	FieldReadyTimeout     = "ready.timeout"
)

// BoundsError reports the first setting outside the route bounds. Its
// message names the field only, never the value, so it can be logged and
// reported as is.
type BoundsError struct {
	Field string
}

func (e *BoundsError) Error() string {
	return "delivery settings outside the deliver-message route bounds: " + e.Field
}

// CheckKey reports whether key is on the allowlist.
func CheckKey(key string) bool {
	_, ok := allowedKeys[key]
	return ok
}

// CheckKeys checks a key list for field: at most MaxKeys keys, each on the
// allowlist. An empty list is accepted here; callers that need keys check
// for that themselves.
func CheckKeys(field string, keys []string) error {
	if len(keys) > MaxKeys {
		return &BoundsError{Field: field}
	}
	for _, k := range keys {
		if !CheckKey(k) {
			return &BoundsError{Field: field}
		}
	}
	return nil
}

// Settings are the resolved harness delivery and readiness settings of one
// agent, as the broker holds them.
type Settings struct {
	SubmitKeys       []string
	InterruptKeys    []string
	ExtraSubmits     int
	ExtraSubmitDelay time.Duration
	// ReadyStrategy is "hook" or "delay" (state.ReadyStrategyHook,
	// state.ReadyStrategyDelay).
	ReadyStrategy string
	ReadySettle   time.Duration
	// ReadyTimeout applies to the hook strategy; zero for delay.
	ReadyTimeout time.Duration
}

// CheckBounds reports whether s fits the route bounds. It returns a
// *BoundsError naming the first field that does not, in a fixed order.
func CheckBounds(s Settings) error {
	if len(s.SubmitKeys) == 0 {
		return &BoundsError{Field: FieldSubmitKeys}
	}
	if err := CheckKeys(FieldSubmitKeys, s.SubmitKeys); err != nil {
		return err
	}
	if len(s.InterruptKeys) == 0 {
		return &BoundsError{Field: FieldInterruptKeys}
	}
	if err := CheckKeys(FieldInterruptKeys, s.InterruptKeys); err != nil {
		return err
	}
	if s.ExtraSubmits < 0 || s.ExtraSubmits > MaxExtraSubmits {
		return &BoundsError{Field: FieldExtraSubmits}
	}
	if s.ExtraSubmitDelay < 0 || s.ExtraSubmitDelay > MaxExtraSubmitDelay {
		return &BoundsError{Field: FieldExtraSubmitDelay}
	}
	switch s.ReadyStrategy {
	case state.ReadyStrategyHook, state.ReadyStrategyDelay:
	default:
		return &BoundsError{Field: FieldReadyStrategy}
	}
	if s.ReadySettle < 0 || s.ReadySettle > MaxReadySettle {
		return &BoundsError{Field: FieldReadySettle}
	}
	if s.ReadyTimeout < 0 || s.ReadyTimeout > MaxReadyTimeout {
		return &BoundsError{Field: FieldReadyTimeout}
	}
	return nil
}

// String returns a short form for tests and debug logs.
func (s Settings) String() string {
	return fmt.Sprintf("submit=%v interrupt=%v extra=%d/%s ready=%s/%s/%s",
		s.SubmitKeys, s.InterruptKeys, s.ExtraSubmits, s.ExtraSubmitDelay,
		s.ReadyStrategy, s.ReadySettle, s.ReadyTimeout)
}
