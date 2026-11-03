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
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Lifecycle of starred messages (web.chat_starred_messages): cleanup when
// a conversation, user or project is deleted, and a bounded periodic
// upkeep pass that repairs stored conversation metadata and purges stars
// whose target is permanently gone. None of it is needed for privacy:
// every read joins the live message and re-checks access, so a cleanup
// that fails never makes deleted content readable; it only leaves an
// unreachable row for the next upkeep pass. All of it runs whether or not
// the experiment is on.

const (
	// messageStarUpkeepTick is how often each replica runs an upkeep pass.
	messageStarUpkeepTick = 10 * time.Minute
	// messageStarUpkeepBatch bounds how many associations one pass examines.
	messageStarUpkeepBatch = 500
	// messageStarUpkeepTimeout bounds one pass.
	messageStarUpkeepTimeout = 5 * time.Second
)

// messageStarUpkeepRuntime is the replica's upkeep loop state.
type messageStarUpkeepRuntime struct {
	mu      sync.Mutex
	stopped bool
	cancel  context.CancelFunc
	running sync.WaitGroup
	// busy is set while a pass runs, so passes never overlap on a replica.
	busy atomic.Bool
	// next, under mu, is where the next pass resumes.
	next StarCleanupPosition
}

// messageStarStore returns the star capability of the chat store, or nil.
func (s *Server) messageStarStore() MessageStarStore {
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return nil
	}
	return messageStarStoreFrom(wcs)
}

// deleteMessageStarsOfConversation deletes the stars of a deleted
// conversation, on its own bounded context so a cancelled request does
// not skip it (finalizeContext). Best effort: one left behind is hidden by
// the read checks and purged by upkeep.
func (s *Server) deleteMessageStarsOfConversation(ctx context.Context, conversationKey string) {
	s.deleteMessageStarsOf(ctx, "conversation", conversationKey, MessageStarStore.DeleteMessageStarsForConversation)
}

// deleteMessageStarsOfUser deletes the stars a deleted user owned. Best
// effort, like deleteMessageStarsOfConversation.
func (s *Server) deleteMessageStarsOfUser(ctx context.Context, userID string) {
	s.deleteMessageStarsOf(ctx, "user", userID, MessageStarStore.DeleteMessageStarsForUser)
}

// deleteMessageStarsOfProject deletes the stars stored under a deleted
// project. Best effort, like deleteMessageStarsOfConversation.
func (s *Server) deleteMessageStarsOfProject(ctx context.Context, projectID string) {
	s.deleteMessageStarsOf(ctx, "project", projectID, MessageStarStore.DeleteMessageStarsForProject)
}

func (s *Server) deleteMessageStarsOf(ctx context.Context, what, id string,
	del func(MessageStarStore, context.Context, string) (int64, error)) {
	stars := s.messageStarStore()
	if stars == nil || id == "" {
		return
	}
	ctx, cancel := finalizeContext(ctx)
	defer cancel()
	n, err := del(stars, ctx, id)
	if err != nil {
		slog.Warn("chat stars: deleting the stars of a deleted "+what+" failed", what+"_id", id, "error", err)
		return
	}
	if n > 0 {
		slog.Info("chat stars: deleted the stars of a deleted "+what, what+"_id", id, "count", n)
	}
}

// startMessageStarUpkeep starts the replica's upkeep loop: one pass every
// messageStarUpkeepTick until ctx is cancelled or stopMessageStarUpkeep
// runs. It runs whenever native chat is enabled, experiment on or off.
func (s *Server) startMessageStarUpkeep(ctx context.Context) {
	if !s.nativeChatEnabled() {
		return
	}
	rt := &s.messageStarUpkeep
	loopCtx, cancel := context.WithCancel(ctx)
	rt.mu.Lock()
	if rt.stopped || rt.cancel != nil {
		rt.mu.Unlock()
		cancel()
		return
	}
	rt.cancel = cancel
	rt.running.Add(1)
	rt.mu.Unlock()
	go func() {
		defer rt.running.Done()
		ticker := time.NewTicker(messageStarUpkeepTick)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				s.messageStarUpkeepPass(loopCtx)
			}
		}
	}()
}

// stopMessageStarUpkeep stops the loop and waits for a pass in progress
// (bounded by messageStarUpkeepTimeout and the cancelled context). It is
// safe to call more than once or without a start; the loop cannot be
// started again afterwards.
func (s *Server) stopMessageStarUpkeep() {
	rt := &s.messageStarUpkeep
	rt.mu.Lock()
	rt.stopped = true
	cancel := rt.cancel
	rt.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	rt.running.Wait()
}

// messageStarUpkeepPass runs one bounded batch from where the last pass
// stopped, wrapping to the start once the scan reaches the end. A pass
// that is still running makes another one a no-op. It reports whether a
// batch ran.
func (s *Server) messageStarUpkeepPass(ctx context.Context) bool {
	rt := &s.messageStarUpkeep
	if !rt.busy.CompareAndSwap(false, true) {
		return false
	}
	defer rt.busy.Store(false)
	stars := s.messageStarStore()
	if stars == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, messageStarUpkeepTimeout)
	defer cancel()
	rt.mu.Lock()
	after := rt.next
	rt.mu.Unlock()
	res, err := stars.PurgeMessageStarCandidates(ctx, after, messageStarUpkeepBatch)
	if err != nil {
		// The position stays: the same batch is tried again next time.
		slog.Warn("chat stars: upkeep pass failed", "error", err)
		return true
	}
	rt.mu.Lock()
	rt.next = res.Next
	rt.mu.Unlock()
	if res.Repaired > 0 || res.Purged > 0 {
		slog.Info("chat stars: upkeep pass", "examined", res.Examined, "repaired", res.Repaired, "purged", res.Purged)
	}
	return true
}
