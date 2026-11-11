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
	"sync"
)

type mockEventPublisher struct {
	mu     sync.Mutex
	events []publishedEvent
	noopEventPublisher
}

func (m *mockEventPublisher) PublishRaw(subject string, data interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, publishedEvent{subject: subject, data: data})
}

func (m *mockEventPublisher) getEvents() []publishedEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]publishedEvent, len(m.events))
	copy(out, m.events)
	return out
}

func (m *mockEventPublisher) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = nil
}

type publishedEvent struct {
	subject string
	data    interface{}
}
