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

package teams

// HealthStatus contains detailed health information for the Teams broker.
type HealthStatus struct {
	Configured    bool   `json:"configured"`
	WebhookActive bool   `json:"webhook_active"`
	StoreReady    bool   `json:"store_ready"`
	PublishLock   string `json:"publish_lock,omitempty"` // "primary", "standby", "disabled"
	QueueDepth    int    `json:"queue_depth"`
}

// DetailedHealth returns the current detailed health status.
func (b *TeamsBroker) DetailedHealth() *HealthStatus {
	b.mu.Lock()
	defer b.mu.Unlock()

	status := &HealthStatus{
		Configured:    b.configured,
		WebhookActive: b.serverRunning,
	}

	if b.store != nil {
		status.StoreReady = true
	}

	if b.publishLock != nil {
		if b.publishLock.Active() {
			status.PublishLock = "primary"
		} else {
			status.PublishLock = "standby"
		}
	} else {
		status.PublishLock = "disabled"
	}

	if b.sendQueue != nil {
		status.QueueDepth = b.sendQueue.Len()
	}

	return status
}
