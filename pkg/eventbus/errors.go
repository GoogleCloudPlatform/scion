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
