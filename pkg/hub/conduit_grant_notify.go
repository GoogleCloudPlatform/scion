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
)

// Grant key changes across hub nodes (design v2.21.27 §3.5, Key-set
// freshness). A rotation or revocation is stored in the shared ring by one
// node; every node caches the ring for conduitGrantKeyRefresh. The node that
// stored the change publishes conduitGrantKeysChangedSubject, and every node
// that receives it drops its cached ring, so the change applies there on the
// next use. The periodic reload bounds a missed event.

// conduitGrantKeysChangedSubject is published after a grant key rotation or
// revocation is stored. The payload names only the key id. It is a
// non-project subject, so it travels on the global channel, and SSE clients
// cannot subscribe to it.
const conduitGrantKeysChangedSubject = "conduit.grant_keys.changed"

// conduitGrantKeysChanged is the payload of conduitGrantKeysChangedSubject.
type conduitGrantKeysChanged struct {
	KeyID string `json:"kid"`
}

// publishConduitGrantKeysChanged announces a stored change to the ring for
// kid. Call it only after the change has been stored, so every node that
// reloads reads the new ring. It is a no-op while hub.conduit is off.
//
// The event is published after the ring write commits, not inside it. A
// node that does not receive it still applies the change: it drops its
// cached ring whenever its LISTEN connection (re)connects, and reloads the
// ring at least every conduitGrantKeyRefresh (1 minute).
func (s *Server) publishConduitGrantKeysChanged(kid string) {
	if s == nil || !s.experimentEnabled(conduitExperiment) {
		return
	}
	s.mu.RLock()
	events := s.events
	s.mu.RUnlock()
	if events == nil {
		return
	}
	events.PublishRaw(conduitGrantKeysChangedSubject, conduitGrantKeysChanged{KeyID: kid})
}

// conduitGrantKeysChangedHere applies a grant key change on this node: it
// drops the cached ring.
func (s *Server) conduitGrantKeysChangedHere() {
	s.conduitGrantKeySet().invalidate()
}

// bindConduitGrantKeyEvents (re)binds this node to grant key changes on the
// current event publisher, for as long as the conduit stream re-check runs:
// the change subscription and, for a publisher with a LISTEN connection, a
// ring reload on every (re)connect, since changes published while it was
// down were not seen. A previous binding is released first. The ring is
// also reloaded once the binding is in place.
func (s *Server) bindConduitGrantKeyEvents() {
	a := s.conduitAuthz.Load()
	if a == nil {
		return
	}
	s.conduitGrantKeysBindMu.Lock()
	if s.conduitGrantKeysUnbind != nil {
		s.conduitGrantKeysUnbind()
		s.conduitGrantKeysUnbind = nil
	}
	s.mu.RLock()
	events := s.events
	s.mu.RUnlock()
	runCtx := a.runContext()
	if events == nil || runCtx.Err() != nil {
		s.conduitGrantKeysBindMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(runCtx)
	var removeListen func()
	if ln, ok := events.(conduitListenNotifier); ok {
		removeListen = ln.AddOnListen(s.conduitGrantKeysChangedHere)
	}
	ch, unsubscribe := events.Subscribe(conduitGrantKeysChangedSubject)
	var once sync.Once
	release := func() {
		once.Do(func() {
			cancel()
			if removeListen != nil {
				removeListen()
			}
			unsubscribe()
		})
	}
	s.conduitGrantKeysUnbind = release
	s.conduitGrantKeysBindMu.Unlock()

	go func() {
		<-ctx.Done()
		release()
	}()
	if ch != nil {
		go s.receiveConduitGrantKeysChanged(ctx, ch)
	}
	s.conduitGrantKeysChangedHere()
}

// receiveConduitGrantKeysChanged applies every grant key change event until
// ctx ends.
func (s *Server) receiveConduitGrantKeysChanged(ctx context.Context, ch <-chan Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			s.conduitGrantKeysChangedHere()
		}
	}
}
