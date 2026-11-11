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
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// recordingChannel is a mock NotificationChannel that records deliveries.
type recordingChannel struct {
	mu         sync.Mutex
	name       string
	deliveries []*messages.StructuredMessage
	returnErr  error
	validErr   error
}

func (r *recordingChannel) Name() string    { return r.name }
func (r *recordingChannel) Validate() error { return r.validErr }
func (r *recordingChannel) Deliver(_ context.Context, msg *messages.StructuredMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deliveries = append(r.deliveries, msg)
	return r.returnErr
}

func (r *recordingChannel) getDeliveries() []*messages.StructuredMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]*messages.StructuredMessage, len(r.deliveries))
	copy(result, r.deliveries)
	return result
}
