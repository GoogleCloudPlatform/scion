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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// PresenceState indicates whether a user is active or idle.
type PresenceState string

const (
	PresenceActive PresenceState = "active"
	PresenceIdle   PresenceState = "idle"
)

const (
	// presenceActiveWindow is the duration within which a heartbeat means
	// the user is "active". After this window without a heartbeat the user
	// transitions to idle. Design §4.5: 2 minutes.
	presenceActiveWindow = 2 * time.Minute

	// presencePersistInterval is the minimum interval between durable
	// User.last_seen writes. Design §4.5: at most once per 5 min.
	presencePersistInterval = 5 * time.Minute

	// presenceSweepInterval is how often the background goroutine checks
	// for idle transitions.
	presenceSweepInterval = 30 * time.Second

	// typingThrottle is the minimum interval between typing events from
	// the same user. Design §4.5: 4 seconds.
	typingThrottle = 4 * time.Second
)

// PresenceEvent is published on project.<id>.chat.presence when a user's
// presence state transitions (active↔idle). Only transitions are published,
// not every heartbeat.
type PresenceEvent struct {
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName"`
	State       string `json:"state"` // "active" or "idle"
}

// TypingEvent is published on project.<id>.chat.typing when a user starts
// typing in a conversation. The frontend handles expiry (6s).
type TypingEvent struct {
	ThreadID    string `json:"threadId"`
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName"`
}

// presenceEntry tracks per-user presence state in memory.
type presenceEntry struct {
	lastHeartbeat time.Time
	lastPersist   time.Time // last time User.last_seen was written to the store
	state         PresenceState
	displayName   string
	projectIDs    []string // projects the user participates in (for SSE fan-out)
}

// typingEntry tracks per-user, per-conversation last-typing timestamp for
// server-side throttling.
type typingEntry struct {
	lastTyping time.Time
}

// PresenceManager maintains an in-memory map of user presence states and
// provides server-side typing throttling. It is single-node only (see design
// §4.5 HA limitation).
type PresenceManager struct {
	mu       sync.RWMutex
	users    map[string]*presenceEntry                // userID -> presence
	typing   map[string]map[string]*typingEntry       // conversationKey -> userID -> typing
	events   EventPublisher
	store    store.Store
	stopCh   chan struct{}
	stopped  chan struct{}
}

// NewPresenceManager creates a new PresenceManager and starts the background
// sweep goroutine that detects idle transitions.
func NewPresenceManager(events EventPublisher, st store.Store) *PresenceManager {
	pm := &PresenceManager{
		users:   make(map[string]*presenceEntry),
		typing:  make(map[string]map[string]*typingEntry),
		events:  events,
		store:   st,
		stopCh:  make(chan struct{}),
		stopped: make(chan struct{}),
	}
	go pm.sweepLoop()
	return pm
}

// Stop shuts down the background sweep goroutine.
func (pm *PresenceManager) Stop() {
	close(pm.stopCh)
	<-pm.stopped
}

// Heartbeat processes a presence heartbeat from a user. It updates the
// in-memory map and publishes a transition event if the state changed.
// projectIDs are the projects the user is currently viewing (for SSE fan-out).
func (pm *PresenceManager) Heartbeat(ctx context.Context, userID, displayName string, projectIDs []string) {
	now := time.Now()

	pm.mu.Lock()
	entry, exists := pm.users[userID]
	if !exists {
		entry = &presenceEntry{
			state: PresenceIdle, // will transition to active below
		}
		pm.users[userID] = entry
	}

	prevState := entry.state
	entry.lastHeartbeat = now
	entry.state = PresenceActive
	entry.displayName = displayName
	entry.projectIDs = projectIDs

	// Persist User.last_seen at most once per presencePersistInterval.
	shouldPersist := now.Sub(entry.lastPersist) >= presencePersistInterval
	if shouldPersist {
		entry.lastPersist = now
	}
	pm.mu.Unlock()

	// Publish transition only when state actually changed.
	if prevState != PresenceActive {
		pm.publishTransition(userID, displayName, PresenceActive, projectIDs)
	}

	// Durable fallback: touch User.last_seen (throttled).
	if shouldPersist && pm.store != nil {
		if err := pm.store.UpdateUserLastSeen(ctx, userID, now); err != nil {
			slog.Warn("failed to persist user last_seen",
				"user_id", userID,
				"error", err)
		}
	}
}

// GetState returns the current presence state for a user.
func (pm *PresenceManager) GetState(userID string) PresenceState {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	entry, exists := pm.users[userID]
	if !exists {
		return PresenceIdle
	}

	// Check if the heartbeat has expired.
	if time.Since(entry.lastHeartbeat) > presenceActiveWindow {
		return PresenceIdle
	}
	return entry.state
}

// GetAllStates returns a map of userID -> presence state for all tracked users.
func (pm *PresenceManager) GetAllStates() map[string]string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	states := make(map[string]string, len(pm.users))
	now := time.Now()
	for uid, entry := range pm.users {
		if now.Sub(entry.lastHeartbeat) > presenceActiveWindow {
			states[uid] = string(PresenceIdle)
		} else {
			states[uid] = string(entry.state)
		}
	}
	return states
}

// RecordTyping checks server-side throttling and returns true if the typing
// event should be published (i.e. enough time has passed since the last one
// from this user in this conversation).
func (pm *PresenceManager) RecordTyping(conversationKey, userID string) bool {
	now := time.Now()

	pm.mu.Lock()
	defer pm.mu.Unlock()

	convMap, exists := pm.typing[conversationKey]
	if !exists {
		convMap = make(map[string]*typingEntry)
		pm.typing[conversationKey] = convMap
	}

	entry, exists := convMap[userID]
	if exists && now.Sub(entry.lastTyping) < typingThrottle {
		return false // throttled
	}

	if !exists {
		entry = &typingEntry{}
		convMap[userID] = entry
	}
	entry.lastTyping = now
	return true
}

// sweepLoop periodically checks for users who have gone idle and publishes
// transition events.
func (pm *PresenceManager) sweepLoop() {
	defer close(pm.stopped)

	ticker := time.NewTicker(presenceSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-pm.stopCh:
			return
		case <-ticker.C:
			pm.sweep()
		}
	}
}

// sweep checks all tracked users and transitions any that have exceeded
// the active window to idle. It also evicts stale typing throttle entries.
func (pm *PresenceManager) sweep() {
	now := time.Now()

	pm.mu.Lock()
	var transitions []struct {
		userID      string
		displayName string
		projectIDs  []string
	}

	for uid, entry := range pm.users {
		if entry.state == PresenceActive && now.Sub(entry.lastHeartbeat) > presenceActiveWindow {
			entry.state = PresenceIdle
			transitions = append(transitions, struct {
				userID      string
				displayName string
				projectIDs  []string
			}{uid, entry.displayName, entry.projectIDs})
		}
	}

	// Evict stale typing throttle entries (older than 2× typingThrottle).
	typingEvictThreshold := 2 * typingThrottle
	for convKey, convMap := range pm.typing {
		for uid, te := range convMap {
			if now.Sub(te.lastTyping) > typingEvictThreshold {
				delete(convMap, uid)
			}
		}
		if len(convMap) == 0 {
			delete(pm.typing, convKey)
		}
	}

	pm.mu.Unlock()

	// Publish transitions outside the lock.
	for _, t := range transitions {
		pm.publishTransition(t.userID, t.displayName, PresenceIdle, t.projectIDs)
	}
}

// publishTransition publishes a presence state change on all relevant
// project subjects.
func (pm *PresenceManager) publishTransition(userID, displayName string, state PresenceState, projectIDs []string) {
	evt := PresenceEvent{
		UserID:      userID,
		DisplayName: displayName,
		State:       string(state),
	}

	for _, pid := range projectIDs {
		pm.events.PublishRaw("project."+pid+".chat.presence", evt)
	}
}

// SeedFromStore seeds the presence map from User.last_seen on cold start.
// Users who were seen within the active window are marked as active.
func (pm *PresenceManager) SeedFromStore(ctx context.Context, st store.Store) {
	users, err := st.ListUsers(ctx, store.UserFilter{}, store.ListOptions{Limit: 1000})
	if err != nil {
		slog.Warn("failed to seed presence from store", "error", err)
		return
	}

	now := time.Now()
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for _, u := range users.Items {
		if u.LastSeen.IsZero() {
			continue
		}
		state := PresenceIdle
		if now.Sub(u.LastSeen) <= presenceActiveWindow {
			state = PresenceActive
		}
		pm.users[u.ID] = &presenceEntry{
			lastHeartbeat: u.LastSeen,
			lastPersist:   u.LastSeen,
			state:         state,
			displayName:   u.DisplayName,
		}
	}
}
