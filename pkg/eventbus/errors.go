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

package eventbus

import "errors"

// ErrEventBusClosed is returned when attempting to publish or subscribe on a closed event bus.
var ErrEventBusClosed = errors.New("event bus is closed")

// ErrSubscriberBufferFull is returned by InProcessEventBus.Publish when a
// message addressed to a user-message topic could not be queued because the
// matching subscriber's buffer was full. Other topics keep the historical
// fire-and-forget behaviour (the message is dropped and Publish returns nil)
// because those callers already tolerate silent drops; the user-message path
// does not, since nothing else re-delivers or surfaces the loss
// (ptone/scion#2311).
var ErrSubscriberBufferFull = errors.New("event bus: subscriber buffer full")

// ErrInProcessPublish wraps a failure of the inprocess spoke inside a
// FanOutEventBus publish. Callers that need to know whether the hub's own
// subscribers received a message (as opposed to an external plugin spoke
// failing) test for it with errors.Is; the underlying cause stays reachable
// too (e.g. ErrEventBusClosed, ErrSubscriberBufferFull).
var ErrInProcessPublish = errors.New("inprocess bus publish failed")

// ErrReservedChannel is returned by FanOutEventBus.Publish when a message
// names a channel reserved for internal use (the inprocess spoke). Nothing
// is published, so callers should treat it as a rejected request rather
// than a channel spoke failure.
var ErrReservedChannel = errors.New("channel is reserved for internal use")

// ErrNilHandler is returned by Subscribe on buses that deliver messages to
// local handlers (InProcessEventBus, FanOutEventBus) when called with a nil
// handler. Accepting one would register a subscription that panics on the
// first delivery (InProcessEventBus) or silently delivers nothing
// (FanOutEventBus). External spokes behind a FanOutEventBus still receive a
// nil handler by design: they only use the pattern for remote-side filtering.
var ErrNilHandler = errors.New("event bus: nil handler")
