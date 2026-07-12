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
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// --- fake EventPublisher for propagation tests ---

// fakeEventPublisher records publishes and supports subscribe for tests.
type fakeEventPublisher struct {
	noopEventPublisher

	mu        sync.Mutex
	published []fakePublishedEvent
	subCh     chan Event
}

type fakePublishedEvent struct {
	Subject string
	Data    interface{}
}

func newFakeEventPublisher() *fakeEventPublisher {
	return &fakeEventPublisher{
		subCh: make(chan Event, 64),
	}
}

func (f *fakeEventPublisher) PublishRaw(subject string, data interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, fakePublishedEvent{Subject: subject, Data: data})

	// Also fan out to the subscription channel so the propagation loop can receive.
	jsonData, err := json.Marshal(data)
	if err != nil {
		return
	}
	select {
	case f.subCh <- Event{Subject: subject, Data: jsonData}:
	default:
	}
}

func (f *fakeEventPublisher) Subscribe(patterns ...string) (<-chan Event, func()) {
	return f.subCh, func() {}
}

func (f *fakeEventPublisher) getPublished() []fakePublishedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakePublishedEvent, len(f.published))
	copy(out, f.published)
	return out
}

// injectEvent injects an event directly into the subscription channel.
func (f *fakeEventPublisher) injectEvent(subject string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	f.subCh <- Event{Subject: subject, Data: jsonData}
}

// --- Tests ---

func TestPublishOnUpdate_EmitsCorrectSubjectPayload(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeEP := newFakeEventPublisher()

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	ops.SetEventPublisher(fakeEP)

	doc := json.RawMessage(`{"admin_emails":["admin@test.com"],"user_access_mode":"open"}`)
	rev, err := ops.Update(context.Background(), "access", doc, "test@user.com", -1, "managed")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	published := fakeEP.getPublished()
	if len(published) == 0 {
		t.Fatal("expected at least one published event")
	}

	// Check subject
	if published[0].Subject != settingsUpdatedSubject {
		t.Errorf("subject: want %q, got %q", settingsUpdatedSubject, published[0].Subject)
	}

	// Check payload
	evt, ok := published[0].Data.(SettingsUpdatedEvent)
	if !ok {
		t.Fatalf("payload type: want SettingsUpdatedEvent, got %T", published[0].Data)
	}
	if evt.Section != "access" {
		t.Errorf("section: want 'access', got %q", evt.Section)
	}
	if evt.Revision != rev {
		t.Errorf("revision: want %d, got %d", rev, evt.Revision)
	}
}

func TestPublishOnUpdate_NoPublishWhenNoEventPublisher(t *testing.T) {
	// SQLite mode: no event publisher set — Update should not panic.
	fakeStore := newFakeHubSettingStore()

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	// No SetEventPublisher call — simulates SQLite mode.

	doc := json.RawMessage(`{"admin_emails":["admin@test.com"]}`)
	_, err := ops.Update(context.Background(), "access", doc, "test@user.com", -1, "managed")
	if err != nil {
		t.Fatalf("Update should not fail without event publisher: %v", err)
	}
}

func TestSubscription_TriggersRefreshAndApply(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["initial@test.com"]}`))

	fakeEP := newFakeEventPublisher()

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	ops.SetEventPublisher(fakeEP)
	_, _ = ops.Refresh(context.Background())

	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ops.StartPropagation(ctx, srv)
	defer ops.StopPropagation()

	// Change the store directly (simulating another replica's write)
	fakeStore.mu.Lock()
	fakeStore.settings["access"].Revision = 2
	fakeStore.settings["access"].Value = json.RawMessage(`{"admin_emails":["updated@test.com"],"user_access_mode":"domain_restricted"}`)
	fakeStore.mu.Unlock()

	// Inject an event to trigger the subscription loop
	fakeEP.injectEvent(settingsUpdatedSubject, SettingsUpdatedEvent{
		Section:  "access",
		Revision: 2,
	})

	// Wait for propagation to apply
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for propagation to apply")
		default:
		}

		srv.mu.RLock()
		emails := srv.config.AdminEmails
		mode := srv.config.UserAccessMode
		srv.mu.RUnlock()

		if len(emails) == 1 && emails[0] == "updated@test.com" && mode == "domain_restricted" {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSubscription_MaintenanceApplied_WithoutEnvOverride(t *testing.T) {
	// When a maintenance row change propagates, it should apply to MaintenanceState
	// on nodes that do NOT have the env override set.
	setEnvForTest(t, "SCION_SERVER_ADMIN_MODE", "")
	setEnvForTest(t, "SCION_SERVER_MAINTENANCE_MESSAGE", "")

	fakeStore := newFakeHubSettingStore()
	fakeEP := newFakeEventPublisher()

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	ops.SetEventPublisher(fakeEP)
	_, _ = ops.Refresh(context.Background())

	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ops.StartPropagation(ctx, srv)
	defer ops.StopPropagation()

	// Add maintenance row to store (simulating another replica enabling maintenance)
	fakeStore.mu.Lock()
	fakeStore.settings["maintenance"] = &store.HubSetting{
		ID:       "maintenance",
		Section:  "maintenance",
		Value:    json.RawMessage(`{"admin_mode":true,"maintenance_message":"System upgrade"}`),
		Revision: 1,
	}
	fakeStore.mu.Unlock()

	// Inject event
	fakeEP.injectEvent(settingsUpdatedSubject, SettingsUpdatedEvent{
		Section:  "maintenance",
		Revision: 1,
	})

	// Wait for maintenance to be applied
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for maintenance to be applied")
		default:
		}

		if srv.maintenance.IsEnabled() && srv.maintenance.Message() == "System upgrade" {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSubscription_MaintenancePropagated_EnvDoesNotOverride(t *testing.T) {
	// In HA mode, propagated maintenance changes apply as-is — env does NOT
	// override (cluster-consistency requirement).
	setEnvForTest(t, "SCION_SERVER_ADMIN_MODE", "true")
	setEnvForTest(t, "SCION_SERVER_MAINTENANCE_MESSAGE", "env-break-glass")

	fakeStore := newFakeHubSettingStore()
	fakeEP := newFakeEventPublisher()

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	ops.SetEventPublisher(fakeEP)
	_, _ = ops.Refresh(context.Background())

	srv := &Server{
		maintenance: NewMaintenanceState(true, "env-break-glass"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ops.StartPropagation(ctx, srv)
	defer ops.StopPropagation()

	// Another replica disables maintenance via DB.
	fakeStore.mu.Lock()
	fakeStore.settings["maintenance"] = &store.HubSetting{
		ID:       "maintenance",
		Section:  "maintenance",
		Value:    json.RawMessage(`{"admin_mode":false,"maintenance_message":"done"}`),
		Revision: 1,
	}
	fakeStore.mu.Unlock()

	fakeEP.injectEvent(settingsUpdatedSubject, SettingsUpdatedEvent{
		Section:  "maintenance",
		Revision: 1,
	})

	// Give time for propagation
	time.Sleep(500 * time.Millisecond)

	// DB wins — env no longer overrides in HA mode.
	if srv.maintenance.IsEnabled() {
		t.Error("maintenance should be disabled (DB propagation wins, env no longer overrides)")
	}
	if srv.maintenance.Message() != "done" {
		t.Errorf("message should be 'done', got %q", srv.maintenance.Message())
	}
}

func TestReconnectCallback_TriggersUnconditionalRefresh(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["initial@test.com"]}`))

	fakeEP := newFakeEventPublisher()

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	ops.SetEventPublisher(fakeEP)
	_, _ = ops.Refresh(context.Background())

	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ops.StartPropagation(ctx, srv)
	defer ops.StopPropagation()

	// Change the store directly (simulating a change missed during connection gap)
	fakeStore.mu.Lock()
	fakeStore.settings["access"].Revision = 2
	fakeStore.settings["access"].Value = json.RawMessage(`{"admin_emails":["reconnect@test.com"]}`)
	fakeStore.mu.Unlock()

	// Simulate reconnect by calling refreshAndApply directly
	// (in a real scenario, the PostgresEventPublisher would call the onReconnect callback)
	ops.refreshAndApply(ctx, srv)

	// Verify the change was applied
	srv.mu.RLock()
	emails := srv.config.AdminEmails
	srv.mu.RUnlock()

	if len(emails) != 1 || emails[0] != "reconnect@test.com" {
		t.Errorf("AdminEmails after reconnect: want [reconnect@test.com], got %v", emails)
	}
}

func TestPollBackstop_DetectsRevisionBump(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["initial@test.com"]}`))

	fakeEP := newFakeEventPublisher()

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	ops.SetEventPublisher(fakeEP)
	_, _ = ops.Refresh(context.Background())

	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}

	// Instead of using the real 60s ticker, we directly test refreshAndApply
	// which is what the backstop calls. This validates the backstop's effect.
	ctx := context.Background()

	// Set server so refreshAndApply has a target
	ops.server = srv

	// Change the store (simulating a change made by another replica)
	fakeStore.mu.Lock()
	fakeStore.settings["access"].Revision = 2
	fakeStore.settings["access"].Value = json.RawMessage(`{"admin_emails":["polled@test.com"],"user_access_mode":"invite_only"}`)
	fakeStore.mu.Unlock()

	// Call refreshAndApply (what the poll backstop does on each tick)
	ops.refreshAndApply(ctx, srv)

	srv.mu.RLock()
	emails := srv.config.AdminEmails
	mode := srv.config.UserAccessMode
	srv.mu.RUnlock()

	if len(emails) != 1 || emails[0] != "polled@test.com" {
		t.Errorf("AdminEmails: want [polled@test.com], got %v", emails)
	}
	if mode != "invite_only" {
		t.Errorf("UserAccessMode: want invite_only, got %s", mode)
	}
}

func TestIdempotency_DoubleApply(t *testing.T) {
	// Applying the same snapshot twice should be harmless.
	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}

	snap := Layer1Snapshot{
		AdminEmails:        []string{"admin@test.com"},
		UserAccessMode:     "domain_restricted",
		AutoSuspendStalled: true,
		AdminMode:          true,
		MaintenanceMessage: "Maintenance",
		HasMaintenanceRow:  true,
	}

	// Apply once
	ApplySnapshot(srv, snap)
	ApplyMaintenanceFromSnapshot(srv, snap)

	// Verify
	if !srv.maintenance.IsEnabled() {
		t.Error("want maintenance enabled after first apply")
	}

	// Apply again — should be idempotent
	ApplySnapshot(srv, snap)
	ApplyMaintenanceFromSnapshot(srv, snap)

	// Verify still the same
	srv.mu.RLock()
	if len(srv.config.AdminEmails) != 1 || srv.config.AdminEmails[0] != "admin@test.com" {
		t.Errorf("AdminEmails after double apply: got %v", srv.config.AdminEmails)
	}
	if srv.config.UserAccessMode != "domain_restricted" {
		t.Errorf("UserAccessMode after double apply: got %s", srv.config.UserAccessMode)
	}
	srv.mu.RUnlock()

	if !srv.maintenance.IsEnabled() {
		t.Error("maintenance should still be enabled after double apply")
	}
	if srv.maintenance.Message() != "Maintenance" {
		t.Errorf("message after double apply: got %q", srv.maintenance.Message())
	}
}

func TestSQLiteMode_NoPublisherNoSubscriptionNoTicker(t *testing.T) {
	// In SQLite/file mode: no event publisher is set, no propagation starts.
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())

	// events should be nil
	if ops.events != nil {
		t.Error("events should be nil in default (SQLite) mode")
	}

	// Update should succeed without publishing
	doc := json.RawMessage(`{"admin_emails":["admin@test.com"]}`)
	_, err := ops.Update(context.Background(), "access", doc, "test@user.com", -1, "managed")
	if err != nil {
		t.Fatalf("Update in SQLite mode should not fail: %v", err)
	}

	// StartPropagation with nil events is a no-op
	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ops.StartPropagation(ctx, srv)
	// No goroutines should have been started
	ops.StopPropagation() // should not hang

	// server should not have been set (no propagation)
	if ops.server != nil {
		t.Error("server should be nil when events are nil (no propagation)")
	}
}

func TestSelfApply_WritingNodeAppliesSynchronously(t *testing.T) {
	// When a node calls Update, it should apply its own change synchronously
	// (without waiting for its own event via subscription).
	fakeStore := newFakeHubSettingStore()
	fakeEP := newFakeEventPublisher()

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	ops.SetEventPublisher(fakeEP)

	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}
	ops.server = srv

	// Update access section
	doc := json.RawMessage(`{"admin_emails":["self-apply@test.com"],"user_access_mode":"invite_only"}`)
	_, err := ops.Update(context.Background(), "access", doc, "test@user.com", -1, "managed")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	// The server config should already be updated (synchronous self-apply)
	srv.mu.RLock()
	emails := srv.config.AdminEmails
	mode := srv.config.UserAccessMode
	srv.mu.RUnlock()

	if len(emails) != 1 || emails[0] != "self-apply@test.com" {
		t.Errorf("AdminEmails after self-apply: want [self-apply@test.com], got %v", emails)
	}
	if mode != "invite_only" {
		t.Errorf("UserAccessMode after self-apply: want invite_only, got %s", mode)
	}
}

func TestSelfApply_MaintenanceAppliedSynchronously(t *testing.T) {
	setEnvForTest(t, "SCION_SERVER_ADMIN_MODE", "")
	setEnvForTest(t, "SCION_SERVER_MAINTENANCE_MESSAGE", "")

	fakeStore := newFakeHubSettingStore()
	fakeEP := newFakeEventPublisher()

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	ops.SetEventPublisher(fakeEP)

	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}
	ops.server = srv

	// Update maintenance section
	doc := json.RawMessage(`{"admin_mode":true,"maintenance_message":"Self-applied maintenance"}`)
	_, err := ops.Update(context.Background(), "maintenance", doc, "admin@test.com", -1, "managed")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Maintenance should be applied synchronously
	if !srv.maintenance.IsEnabled() {
		t.Error("maintenance should be enabled after self-apply")
	}
	if srv.maintenance.Message() != "Self-applied maintenance" {
		t.Errorf("message: want 'Self-applied maintenance', got %q", srv.maintenance.Message())
	}
}

func TestConcurrentPropagation(t *testing.T) {
	// Race test: concurrent Update, Refresh, and Snapshot calls.
	// Run with -race to detect data races.
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["a@b.com"]}`))

	fakeEP := newFakeEventPublisher()

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	ops.SetEventPublisher(fakeEP)
	_, _ = ops.Refresh(context.Background())

	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}
	ops.server = srv

	var wg sync.WaitGroup
	const goroutines = 5
	const iterations = 50

	ctx := context.Background()

	// Concurrent Updates
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				doc := json.RawMessage(`{"admin_emails":["concurrent@test.com"]}`)
				_, _ = ops.Update(ctx, "access", doc, "test", -1, "managed")
			}
		}()
	}

	// Concurrent Refreshes
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_, _ = ops.Refresh(ctx)
			}
		}()
	}

	// Concurrent Snapshots
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				snap := ops.Snapshot()
				_ = snap.AdminEmails
				_ = snap.EnvOverrides
			}
		}()
	}

	wg.Wait()
}
