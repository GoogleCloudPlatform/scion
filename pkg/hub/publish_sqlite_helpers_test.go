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

//go:build !no_sqlite

package hub

import (
	"context"
	"errors"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// spyEventPublisher embeds noopEventPublisher and records PublishUserMessage
// calls.
type spyEventPublisher struct {
	noopEventPublisher
	mu       sync.Mutex
	userMsgs []*store.Message
}

// stubWebChatStore embeds the WebChatStore interface so that only the
// methods actually exercised need a real implementation. Unimplemented
// methods panic with a nil-receiver dereference, which is the desired
// signal in a test.
type stubWebChatStore struct {
	WebChatStore
}

// createMessageFailStore wraps a real store and makes CreateMessage return an error.
type createMessageFailStore struct {
	store.Store
}

func (s *spyEventPublisher) PublishUserMessage(_ context.Context, msg *store.Message, _ []AttachmentRef, _ []artifacts.MessageRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userMsgs = append(s.userMsgs, msg)
}

func (s *spyEventPublisher) getUserMessages() []*store.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*store.Message, len(s.userMsgs))
	copy(out, s.userMsgs)
	return out
}

func (s *createMessageFailStore) CreateMessage(_ context.Context, _ *store.Message) error {
	return errors.New("injected CreateMessage failure")
}
