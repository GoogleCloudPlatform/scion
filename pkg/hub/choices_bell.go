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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Decisions bell writer, debounce queue, topic fan-out, and reconciliation
// (ptone/scion#7997, parent ptone/scion#7989; design B1-B8, C3).

var decisionsWaitingNotifyNamespace = uuid.MustParse("3e9a5070-13a9-4a5d-8661-7df38bd78c93")

const (
	// defaultDecisionBellDebounce is the per-user/topic coalescing window (design B5).
	defaultDecisionBellDebounce = 2 * time.Second
	// defaultDecisionBellWorkers bounds concurrent bell recomputes per hub process (design B5).
	defaultDecisionBellWorkers = 2
	// maxDecisionBellTopicFanoutMembers is the project member cap above which
	// immediate topic fan-out is skipped in favor of reconcile-on-open (design B8).
	maxDecisionBellTopicFanoutMembers = 500
	// decisionsWaitingClearedMessage is stored when N == 0 because the Ent
	// Notification.message schema field is NotEmpty.
	decisionsWaitingClearedMessage = "No decisions are waiting on you"
	// decisionBellStopGrace bounds how long Server shutdown waits for in-flight workers.
	decisionBellStopGrace = 5 * time.Second
)

// decisionsWaitingNotificationID returns the deterministic UUIDv5 notification
// row ID for userID's coalesced Decisions bell item (design B1).
func decisionsWaitingNotificationID(userID string) string {
	return uuid.NewSHA1(decisionsWaitingNotifyNamespace, []byte("decisions_waiting:"+userID)).String()
}

// formatDecisionsWaitingMessage formats the coalesced bell summary (design B1).
func formatDecisionsWaitingMessage(total, projects int) string {
	if total <= 0 {
		return decisionsWaitingClearedMessage
	}
	head := fmt.Sprintf("%d decisions are waiting on you", total)
	if total == 1 {
		head = "1 decision is waiting on you"
	}
	if projects <= 1 {
		return head + " in 1 project"
	}
	return fmt.Sprintf("%s in %d projects", head, projects)
}

// decisionsBellEnabled reports whether both web.project_decisions and
// web.chat_choices are enabled (design C3).
func (s *Server) decisionsBellEnabled() bool {
	if s == nil {
		return false
	}
	return s.experimentEnabled(experiments.ProjectDecisions) && s.experimentEnabled(experiments.ChatChoices)
}

type decisionBellTopicKey struct {
	projectID string
	chatKey   string
}

type decisionBellUserEntry struct {
	dueAt     time.Time
	reconcile bool
}

type decisionBellWorkItem struct {
	isTopic   bool
	topic     decisionBellTopicKey
	userID    string
	reconcile bool
}

// decisionBellState manages the in-process debounce queue and bounded worker
// pool for coalesced Decisions bell updates on Server. Zero value is ready for
// lazy startup on first enqueue.
type decisionBellState struct {
	mu              sync.Mutex
	started         bool
	stopped         bool
	stopCh          chan struct{}
	wakeCh          chan struct{}
	idleCond        *sync.Cond
	wg              sync.WaitGroup
	debounce        time.Duration
	maxTopicMembers int

	userDue        map[string]decisionBellUserEntry
	topicDue       map[decisionBellTopicKey]time.Time
	inFlightUsers  map[string]bool
	inFlightTopics map[decisionBellTopicKey]bool
}

func (s *Server) decisionBellDebounce() time.Duration {
	s.decisionBell.mu.Lock()
	d := s.decisionBell.debounce
	s.decisionBell.mu.Unlock()
	if d > 0 {
		return d
	}
	return defaultDecisionBellDebounce
}

func (s *Server) decisionBellTopicFanoutCap() int {
	s.decisionBell.mu.Lock()
	c := s.decisionBell.maxTopicMembers
	s.decisionBell.mu.Unlock()
	if c > 0 {
		return c
	}
	return maxDecisionBellTopicFanoutMembers
}

// ensureDecisionBellStartedLocked initializes maps and starts the worker pool.
// Caller must hold s.decisionBell.mu.
func (s *Server) ensureDecisionBellStartedLocked() bool {
	st := &s.decisionBell
	if st.stopped {
		return false
	}
	if st.started {
		return true
	}
	st.started = true
	st.stopCh = make(chan struct{})
	st.wakeCh = make(chan struct{}, 1)
	st.idleCond = sync.NewCond(&st.mu)
	st.userDue = make(map[string]decisionBellUserEntry)
	st.topicDue = make(map[decisionBellTopicKey]time.Time)
	st.inFlightUsers = make(map[string]bool)
	st.inFlightTopics = make(map[decisionBellTopicKey]bool)
	for i := 0; i < defaultDecisionBellWorkers; i++ {
		st.wg.Add(1)
		go s.runDecisionBellWorker()
	}
	return true
}

func (st *decisionBellState) signalWakeLocked() {
	if st.wakeCh == nil {
		return
	}
	select {
	case st.wakeCh <- struct{}{}:
	default:
	}
}

func (st *decisionBellState) isIdleLocked() bool {
	return len(st.userDue) == 0 &&
		len(st.topicDue) == 0 &&
		len(st.inFlightUsers) == 0 &&
		len(st.inFlightTopics) == 0
}

func (st *decisionBellState) notifyIdleIfEmptyLocked() {
	if st.idleCond != nil && st.isIdleLocked() {
		st.idleCond.Broadcast()
	}
}

// stopDecisionBell stops the worker pool on Server shutdown.
func (s *Server) stopDecisionBell() {
	if s == nil {
		return
	}
	st := &s.decisionBell
	st.mu.Lock()
	if st.stopped {
		st.mu.Unlock()
		return
	}
	st.stopped = true
	wasStarted := st.started
	stopCh := st.stopCh
	if st.idleCond != nil {
		st.idleCond.Broadcast()
	}
	st.mu.Unlock()

	if !wasStarted {
		return
	}
	close(stopCh)
	done := make(chan struct{})
	go func() {
		st.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(decisionBellStopGrace):
	}
}

// enqueueDecisionBellEvent schedules bell recomputes after a decision lifecycle
// transition (prompt created, answered, interpreted, confirmed, changed, batch
// answered, or replaced; design B4).
func (s *Server) enqueueDecisionBellEvent(projectID, threadID, namedUserID string) {
	if s == nil || s.store == nil || !s.decisionsBellEnabled() {
		return
	}
	namedUserID = strings.TrimSpace(namedUserID)
	if namedUserID != "" {
		s.enqueueDecisionBellUser(namedUserID, false)
	}
	if strings.HasPrefix(threadID, "dm:") {
		parts := strings.Split(threadID, ":")
		if len(parts) >= 5 {
			for i, id := range []string{parts[2], parts[4]} {
				kind := parts[1]
				if i == 1 {
					kind = parts[3]
				}
				if kind == "user" && id != "" && id != namedUserID {
					s.enqueueDecisionBellUser(id, false)
				}
			}
		}
		return
	}
	if projectID != "" && threadID != "" && !strings.HasPrefix(threadID, "agent:") {
		s.enqueueDecisionBellTopic(projectID, threadID)
	}
}

func (s *Server) enqueueDecisionBellUser(userID string, reconcile bool) {
	userID = strings.TrimSpace(userID)
	if s == nil || s.store == nil || userID == "" {
		return
	}
	debounce := s.decisionBellDebounce()
	now := time.Now()
	dueAt := now.Add(debounce)
	if reconcile {
		dueAt = now
	}

	st := &s.decisionBell
	st.mu.Lock()
	if !s.ensureDecisionBellStartedLocked() {
		st.mu.Unlock()
		return
	}
	if existing, ok := st.userDue[userID]; ok {
		if dueAt.Before(existing.dueAt) {
			existing.dueAt = dueAt
		}
		if reconcile {
			existing.reconcile = true
		}
		st.userDue[userID] = existing
	} else {
		st.userDue[userID] = decisionBellUserEntry{
			dueAt:     dueAt,
			reconcile: reconcile,
		}
	}
	st.signalWakeLocked()
	st.mu.Unlock()
}

func (s *Server) enqueueDecisionBellTopic(projectID, chatKey string) {
	if s == nil || s.store == nil || projectID == "" || chatKey == "" {
		return
	}
	debounce := s.decisionBellDebounce()
	dueAt := time.Now().Add(debounce)
	tk := decisionBellTopicKey{projectID: projectID, chatKey: chatKey}

	st := &s.decisionBell
	st.mu.Lock()
	if !s.ensureDecisionBellStartedLocked() {
		st.mu.Unlock()
		return
	}
	if existing, ok := st.topicDue[tk]; !ok || dueAt.Before(existing) {
		if !ok {
			st.topicDue[tk] = dueAt
		}
	}
	st.signalWakeLocked()
	st.mu.Unlock()
}

// reconcileDecisionBellAsync schedules an immediate (0-delay) background
// reconciliation for userID (design B6, C3).
func (s *Server) reconcileDecisionBellAsync(userID string) {
	s.enqueueDecisionBellUser(userID, true)
}

func (s *Server) runDecisionBellWorker() {
	st := &s.decisionBell
	defer st.wg.Done()

	var timer *time.Timer
	stopTimer := func() {
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}
	defer stopTimer()

	for {
		item, nextWait, ok := s.nextDecisionBellWorkItem()
		if ok {
			s.executeDecisionBellWorkItem(item)
			continue
		}

		stopTimer()
		var timerCh <-chan time.Time
		if nextWait >= 0 {
			if timer == nil {
				timer = time.NewTimer(nextWait)
			} else {
				timer.Reset(nextWait)
			}
			timerCh = timer.C
		}

		select {
		case <-st.stopCh:
			return
		case <-st.wakeCh:
		case <-timerCh:
		}
	}
}

// nextDecisionBellWorkItem pops one ready work item whose key is not already
// in flight, or returns (zero, waitDuration, false) where waitDuration < 0
// means nothing is queued.
func (s *Server) nextDecisionBellWorkItem() (decisionBellWorkItem, time.Duration, bool) {
	st := &s.decisionBell
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.stopped {
		return decisionBellWorkItem{}, -1, false
	}

	now := time.Now()
	var earliest time.Time

	// Check topics first so ready topic expansions feed per-user recomputes.
	for tk, dueAt := range st.topicDue {
		if st.inFlightTopics[tk] {
			continue
		}
		if !now.Before(dueAt) {
			delete(st.topicDue, tk)
			st.inFlightTopics[tk] = true
			return decisionBellWorkItem{isTopic: true, topic: tk}, 0, true
		}
		if earliest.IsZero() || dueAt.Before(earliest) {
			earliest = dueAt
		}
	}

	for uid, entry := range st.userDue {
		if st.inFlightUsers[uid] {
			continue
		}
		if !now.Before(entry.dueAt) {
			delete(st.userDue, uid)
			st.inFlightUsers[uid] = true
			return decisionBellWorkItem{
				userID:    uid,
				reconcile: entry.reconcile,
			}, 0, true
		}
		if earliest.IsZero() || entry.dueAt.Before(earliest) {
			earliest = entry.dueAt
		}
	}

	if earliest.IsZero() {
		return decisionBellWorkItem{}, -1, false
	}
	return decisionBellWorkItem{}, time.Until(earliest), false
}

func (s *Server) executeDecisionBellWorkItem(item decisionBellWorkItem) {
	st := &s.decisionBell
	if item.isTopic {
		var members []string
		if s.decisionsBellEnabled() {
			ctx, cancel := context.WithTimeout(context.Background(), s.chatSearchTimeout())
			members = s.expandDecisionBellTopic(ctx, item.topic.projectID, item.topic.chatKey)
			cancel()
		}

		now := time.Now()
		st.mu.Lock()
		if !st.stopped {
			for _, uid := range members {
				if _, exists := st.userDue[uid]; !exists {
					st.userDue[uid] = decisionBellUserEntry{dueAt: now}
				}
			}
			if len(members) > 0 || st.topicDue[item.topic] != (time.Time{}) {
				st.signalWakeLocked()
			}
		}
		delete(st.inFlightTopics, item.topic)
		st.notifyIdleIfEmptyLocked()
		st.mu.Unlock()
		return
	}

	func() {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("decisions bell: worker panic", "user_id", item.userID, "panic", fmt.Sprint(rec))
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), s.chatSearchTimeout())
		defer cancel()

		if !s.decisionsBellEnabled() {
			if item.reconcile {
				if err := s.acknowledgeDecisionsWaitingIfPresent(ctx, item.userID); err != nil {
					slog.Warn("decisions bell: flag-off reconcile failed", "user_id", item.userID, "error", err)
				}
			}
			return
		}
		if err := s.recomputeDecisionBellUser(ctx, item.userID); err != nil {
			slog.Warn("decisions bell: recompute failed", "user_id", item.userID, "error", err)
		}
	}()

	st.mu.Lock()
	delete(st.inFlightUsers, item.userID)
	if _, pendingAgain := st.userDue[item.userID]; pendingAgain {
		st.signalWakeLocked()
	}
	st.notifyIdleIfEmptyLocked()
	st.mu.Unlock()
}

// recomputeDecisionBellUser evaluates waitingOnYouTotal for userID and applies
// the B2 notification state transitions.
func (s *Server) recomputeDecisionBellUser(ctx context.Context, userID string) error {
	if s == nil || s.store == nil || !s.decisionsBellEnabled() {
		return nil
	}
	total, projects, err := s.waitingOnYouTotal(ctx, userID)
	if err != nil {
		return err
	}
	return s.writeDecisionsWaitingNotification(ctx, userID, total, projects)
}

// writeDecisionsWaitingNotification writes or updates userID's deterministic
// decisions_waiting notification row according to B2 and publishes any change
// on user.<userID>.notification only (never external channels or brokers).
func (s *Server) writeDecisionsWaitingNotification(ctx context.Context, userID string, total, projects int) error {
	userID = strings.TrimSpace(userID)
	if s == nil || s.store == nil || userID == "" {
		return nil
	}
	id := decisionsWaitingNotificationID(userID)

	for attempt := 0; attempt < 2; attempt++ {
		existing, err := s.store.GetNotification(ctx, id)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if errors.Is(err, store.ErrNotFound) {
			existing = nil
		}

		prevCount := 0
		if existing != nil && existing.ResourceID != "" {
			if p, perr := strconv.Atoi(existing.ResourceID); perr == nil && p >= 0 {
				prevCount = p
			}
		}

		if total <= 0 {
			if existing == nil || (existing.Acknowledged && existing.ResourceID == "0") {
				return nil
			}
			if err := s.store.UpdateNotificationState(ctx, id, decisionsWaitingClearedMessage, "0", true); err != nil {
				return err
			}
			s.publishDecisionsWaitingUserEvent(ctx, id)
			return nil
		}

		msg := formatDecisionsWaitingMessage(total, projects)
		resID := strconv.Itoa(total)

		if total > prevCount {
			if existing == nil {
				notif := &store.Notification{
					ID:             id,
					SubscriptionID: store.CreatorFallbackSubscriptionID,
					AgentID:        uuid.Nil.String(),
					ProjectID:      uuid.Nil.String(),
					SubscriberType: store.SubscriberTypeUser,
					SubscriberID:   userID,
					Status:         store.NotificationStatusDecisionsWaiting,
					Message:        msg,
					Kind:           store.NotificationKindDecisionsWaiting,
					ResourceType:   store.NotificationResourceDecisions,
					ResourceID:     resID,
					Acknowledged:   false,
					CreatedAt:      time.Now().UTC(),
				}
				if err := s.store.CreateNotification(ctx, notif); err != nil {
					if errors.Is(err, store.ErrAlreadyExists) && attempt == 0 {
						continue
					}
					return err
				}
				s.publishDecisionsWaitingUserEvent(ctx, id)
				return nil
			}
			if err := s.store.UpdateNotificationState(ctx, id, msg, resID, false); err != nil {
				return err
			}
			s.publishDecisionsWaitingUserEvent(ctx, id)
			return nil
		}

		// 0 < total <= prevCount: keep existing.Acknowledged (design B2).
		if existing.ResourceID == resID && existing.Message == msg {
			return nil
		}
		if err := s.store.UpdateNotificationState(ctx, id, msg, resID, existing.Acknowledged); err != nil {
			return err
		}
		s.publishDecisionsWaitingUserEvent(ctx, id)
		return nil
	}
	return nil
}

// acknowledgeDecisionsWaitingIfPresent acknowledges an existing unacknowledged
// decisions_waiting row when the feature flag is off (design C3).
func (s *Server) acknowledgeDecisionsWaitingIfPresent(ctx context.Context, userID string) error {
	userID = strings.TrimSpace(userID)
	if s == nil || s.store == nil || userID == "" {
		return nil
	}
	id := decisionsWaitingNotificationID(userID)
	existing, err := s.store.GetNotification(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if existing == nil || existing.Acknowledged {
		return nil
	}
	if err := s.store.UpdateNotificationState(ctx, id, decisionsWaitingClearedMessage, "0", true); err != nil {
		return err
	}
	s.publishDecisionsWaitingUserEvent(ctx, id)
	return nil
}

func (s *Server) publishDecisionsWaitingUserEvent(ctx context.Context, id string) {
	if s == nil || s.events == nil || s.store == nil {
		return
	}
	notif, err := s.store.GetNotification(ctx, id)
	if err != nil || notif == nil {
		return
	}
	s.events.PublishUserNotification(ctx, notif)
}

// decisionBellProjectMemberIDs returns the distinct candidate user IDs that hold
// active membership-conferring role bindings (direct or via group closure) on
// projectID. Each candidate is still re-checked with CheckEffectiveMembership
// and chatUserAccess before topic fan-out and inside waitingOnYouTotal.
func (s *Server) decisionBellProjectMemberIDs(ctx context.Context, projectID string) ([]string, error) {
	bindings, err := s.store.ListRoleBindingsForScope(ctx, store.RoleScopeProject, projectID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	active := make([]*store.RoleBinding, 0, len(bindings))
	for _, b := range bindings {
		if b == nil || b.ScopeType != store.RoleScopeProject || b.ScopeID != projectID || !isBindingActive(b, now) {
			continue
		}
		active = append(active, b)
	}
	if len(active) == 0 {
		return nil, nil
	}

	if s.authzService != nil {
		active, err = membershipBindings(active, s.authzService.roleDefsLoader(ctx))
		if err != nil {
			return nil, err
		}
	} else {
		defs, derr := s.store.GetRoleDefinitionsByIDs(ctx, collectRoleDefinitionIDs(active))
		if derr != nil {
			return nil, derr
		}
		filtered := active[:0]
		for _, b := range active {
			if rd := defs[b.RoleDefinitionID]; rd != nil && rd.ConfersProjectMembership() {
				filtered = append(filtered, b)
			}
		}
		active = filtered
	}

	seen := make(map[string]bool, len(active))
	var out []string
	for _, b := range active {
		switch b.PrincipalType {
		case store.RoleBindingPrincipalUser:
			if b.PrincipalID != "" && !seen[b.PrincipalID] {
				seen[b.PrincipalID] = true
				out = append(out, b.PrincipalID)
			}
		case store.RoleBindingPrincipalGroup:
			groupUsers, gerr := transitiveGroupMemberUsers(ctx, s.store, b.PrincipalID)
			if gerr != nil {
				return nil, gerr
			}
			for _, uid := range groupUsers {
				if uid != "" && !seen[uid] {
					seen[uid] = true
					out = append(out, uid)
				}
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// expandDecisionBellTopic resolves the active project members who can post in
// topic chatKey, skipping fan-out when the project exceeds the B8 member cap.
func (s *Server) expandDecisionBellTopic(ctx context.Context, projectID, chatKey string) []string {
	if s == nil || s.store == nil || projectID == "" || chatKey == "" {
		return nil
	}
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return nil
	}

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil || project == nil {
		return nil
	}
	topic, err := wcs.GetTopic(ctx, chatKey)
	if err != nil || topic == nil || topic.DeletedAt != nil || topic.ProjectID != projectID {
		return nil
	}

	candidateIDs, err := s.decisionBellProjectMemberIDs(ctx, projectID)
	if err != nil || len(candidateIDs) == 0 {
		return nil
	}

	capMembers := s.decisionBellTopicFanoutCap()
	if len(candidateIDs) > capMembers {
		slog.Info("decisions bell: skipping topic fan-out over project member cap",
			"project_id", projectID,
			"chat_key", chatKey,
			"member_count", len(candidateIDs),
			"cap", capMembers,
		)
		return nil
	}

	ctx = withAuthzInputMemo(ctx)
	posting := make([]string, 0, len(candidateIDs))
	for _, uid := range candidateIDs {
		u, uerr := checkUserAccount(ctx, s.store, PrincipalContext{Kind: PrincipalKindUser, ID: uid})
		if classifyUserAccount(u, uerr) != accountAllowed || u == nil {
			continue
		}
		mem := s.CheckEffectiveMembership(ctx, uid, projectID)
		if mem.Err != nil || !mem.IsMember {
			continue
		}
		viewer := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb))
		access := s.chatUserAccess(ctx, viewer, project, []string{ChatActionRead}, chatThreadWriteActions, nil, false, false)
		if access.Can(ChatActionRead) && access.Can(ChatActionPost) {
			posting = append(posting, uid)
		}
	}
	return posting
}

type decisionBellStatusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *decisionBellStatusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *decisionBellStatusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *decisionBellStatusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func decisionsBellReconcileCaller(r *http.Request) string {
	ctx := r.Context()
	if GetAgentIdentityFromContext(ctx) != nil {
		return ""
	}
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) || isVisitorIdentity(identity) {
		return ""
	}
	cls, _ := inboxCredentialOf(ctx)
	if cls != inboxCredentialSession && cls != inboxCredentialToken {
		return ""
	}
	user, ok := identity.(UserIdentity)
	if !ok {
		return ""
	}
	return strings.TrimSpace(user.ID())
}

// withDecisionsBellReconcile wraps a Decisions HTTP handler (profile or project)
// so GET requests for tab=pending reconcile the caller's bell notification row
// asynchronously after the HTTP response has been written (design B6, C3).
func (s *Server) withDecisionsBellReconcile(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			next(w, r)
			return
		}
		rec := &decisionBellStatusRecorder{ResponseWriter: w}
		next(rec, r)

		tab := strings.TrimSpace(r.URL.Query().Get("tab"))
		if tab != "" && tab != ProjectDecisionsTabPending {
			return
		}
		userID := decisionsBellReconcileCaller(r)
		if userID == "" {
			return
		}
		if !s.decisionsBellEnabled() {
			s.reconcileDecisionBellAsync(userID)
			return
		}
		if rec.status == http.StatusOK {
			s.reconcileDecisionBellAsync(userID)
		}
	}
}
