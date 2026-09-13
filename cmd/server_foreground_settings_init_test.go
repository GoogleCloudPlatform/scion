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

package cmd

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// --- test doubles ---

// backfillFailStore wraps a real store.Store and overrides BackfillOrigin to
// return an error, simulating a persistent DB failure during settings init.
// All other Store/HubSettingStore methods delegate to the underlying store.
type backfillFailStore struct {
	store.Store
	calls atomic.Int32
}

func (s *backfillFailStore) BackfillOrigin(_ context.Context) error {
	s.calls.Add(1)
	return errors.New("simulated DB connection failure")
}

// backfillTransientFailStore wraps a real store.Store and fails BackfillOrigin
// for the first N calls, then delegates to the underlying store.
type backfillTransientFailStore struct {
	store.Store
	calls     atomic.Int32
	failCount int32
}

func (s *backfillTransientFailStore) BackfillOrigin(ctx context.Context) error {
	n := s.calls.Add(1)
	if n <= s.failCount {
		return errors.New("transient DB error")
	}
	return s.Store.(store.HubSettingStore).BackfillOrigin(ctx)
}

// --- tests ---

// TestInitOperationalSettings_FailClosed verifies that when the settings
// store is persistently broken, initOperationalSettingsWithRetry returns a
// non-nil error. Because the initHubServer caller does log.Fatalf on error,
// this error prevents the hub from accepting traffic — the fail-closed
// invariant for DB-persisted maintenance mode (admin_mode=true).
func TestInitOperationalSettings_FailClosed(t *testing.T) {
	ctx := context.Background()
	cfg := &config.GlobalConfig{}
	cfg.Database.Driver = "postgres"

	inner := newTestStore(t)

	// Use the real store for hub.New (it needs full store capabilities),
	// but pass the failing wrapper to initOperationalSettingsWithRetry
	// (which only uses HubSettingStore methods after the type assertion).
	srv, err := hub.New(hub.ServerConfig{}, inner)
	if err != nil {
		t.Fatalf("hub.New: %v", err)
	}

	fakeStore := &backfillFailStore{Store: inner}
	err = initOperationalSettingsWithRetry(ctx, cfg, srv, fakeStore, t.TempDir())
	if err == nil {
		t.Fatal("expected initOperationalSettingsWithRetry to return an error when settings load fails, but got nil — server would accept traffic without authoritative settings (maintenance-mode bypass)")
	}

	// Verify retries actually happened.
	calls := fakeStore.calls.Load()
	expectedMinCalls := int32(settingsInitMaxRetries + 1) // initial + retries
	if calls < expectedMinCalls {
		t.Errorf("BackfillOrigin called %d times, want at least %d (1 initial + %d retries)",
			calls, expectedMinCalls, settingsInitMaxRetries)
	}

	// Verify operational settings were NOT set on the server.
	if srv.GetOperationalSettings() != nil {
		t.Error("OperationalSettings should be nil when init fails — server must not have settings from a partial init")
	}

	t.Logf("initOperationalSettingsWithRetry correctly returned error after %d attempts: %v", calls, err)
}

// TestInitOperationalSettings_SuccessOnRetry verifies that a transient
// failure followed by recovery allows settings init to succeed.
func TestInitOperationalSettings_SuccessOnRetry(t *testing.T) {
	ctx := context.Background()
	cfg := &config.GlobalConfig{}
	cfg.Database.Driver = "sqlite3"

	inner := newTestStore(t)

	srv, err := hub.New(hub.ServerConfig{}, inner)
	if err != nil {
		t.Fatalf("hub.New: %v", err)
	}

	fakeStore := &backfillTransientFailStore{
		Store:     inner,
		failCount: 2, // fail first 2 calls, succeed on 3rd
	}

	err = initOperationalSettingsWithRetry(ctx, cfg, srv, fakeStore, t.TempDir())
	if err != nil {
		t.Fatalf("expected success after transient failures, got: %v", err)
	}

	// Verify the hub now has operational settings set.
	if srv.GetOperationalSettings() == nil {
		t.Error("OperationalSettings is nil after successful init — ApplySnapshot/SetOperationalSettings was not called")
	}
}

// TestInitOperationalSettings_ContextCancelled verifies that a cancelled
// context causes the retry loop to exit promptly.
func TestInitOperationalSettings_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	cfg := &config.GlobalConfig{}
	cfg.Database.Driver = "postgres"

	inner := newTestStore(t)
	srv, err := hub.New(hub.ServerConfig{}, inner)
	if err != nil {
		t.Fatalf("hub.New: %v", err)
	}

	fakeStore := &backfillFailStore{Store: inner}
	err = initOperationalSettingsWithRetry(ctx, cfg, srv, fakeStore, t.TempDir())
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
}
