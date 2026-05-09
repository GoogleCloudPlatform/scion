package state

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("creating test store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSetAgentSubscription_CrossGroveIsolation(t *testing.T) {
	s := newTestStore(t)

	// Same user subscribes to same-named agent in two groves.
	subA := &AgentSubscription{
		PlatformUserID: "user-1",
		Platform:       "googlechat",
		AgentID:        "deploy",
		ProjectID:        "grove-A",
		Activities:     "COMPLETED",
	}
	subB := &AgentSubscription{
		PlatformUserID: "user-1",
		Platform:       "googlechat",
		AgentID:        "deploy",
		ProjectID:        "grove-B",
		Activities:     "ERROR",
	}

	if err := s.SetAgentSubscription(subA); err != nil {
		t.Fatalf("set subA: %v", err)
	}
	if err := s.SetAgentSubscription(subB); err != nil {
		t.Fatalf("set subB: %v", err)
	}

	// Both subscriptions must coexist.
	gotA, err := s.GetAgentSubscription("user-1", "googlechat", "deploy", "grove-A")
	if err != nil {
		t.Fatalf("get subA: %v", err)
	}
	if gotA == nil {
		t.Fatal("grove-A subscription was overwritten by grove-B insert")
	}
	if gotA.Activities != "COMPLETED" {
		t.Errorf("grove-A activities = %q, want %q", gotA.Activities, "COMPLETED")
	}

	gotB, err := s.GetAgentSubscription("user-1", "googlechat", "deploy", "grove-B")
	if err != nil {
		t.Fatalf("get subB: %v", err)
	}
	if gotB == nil {
		t.Fatal("grove-B subscription missing")
	}
	if gotB.Activities != "ERROR" {
		t.Errorf("grove-B activities = %q, want %q", gotB.Activities, "ERROR")
	}
}

func TestDeleteAgentSubscription_GroveScoped(t *testing.T) {
	s := newTestStore(t)

	// Same user, same agent, two groves.
	subA := &AgentSubscription{
		PlatformUserID: "user-1",
		Platform:       "googlechat",
		AgentID:        "deploy",
		ProjectID:        "grove-A",
		Activities:     "COMPLETED",
	}
	subB := &AgentSubscription{
		PlatformUserID: "user-1",
		Platform:       "googlechat",
		AgentID:        "deploy",
		ProjectID:        "grove-B",
		Activities:     "ERROR",
	}

	if err := s.SetAgentSubscription(subA); err != nil {
		t.Fatalf("set subA: %v", err)
	}
	if err := s.SetAgentSubscription(subB); err != nil {
		t.Fatalf("set subB: %v", err)
	}

	// Delete grove-A subscription only.
	if err := s.DeleteAgentSubscription("user-1", "googlechat", "deploy", "grove-A"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Project-A subscription should be gone.
	got, err := s.GetAgentSubscription("user-1", "googlechat", "deploy", "grove-A")
	if err != nil {
		t.Fatalf("get subA after delete: %v", err)
	}
	if got != nil {
		t.Errorf("expected grove-A subscription to be deleted, got %+v", got)
	}

	// Project-B subscription must be untouched.
	got, err = s.GetAgentSubscription("user-1", "googlechat", "deploy", "grove-B")
	if err != nil {
		t.Fatalf("get subB after delete: %v", err)
	}
	if got == nil {
		t.Fatal("grove-B subscription should still exist")
	}
	if got.Activities != "ERROR" {
		t.Errorf("grove-B activities = %q, want %q", got.Activities, "ERROR")
	}

	// Deleting with wrong grove_id should not remove anything.
	if err := s.DeleteAgentSubscription("user-1", "googlechat", "deploy", "grove-WRONG"); err != nil {
		t.Fatalf("delete with wrong grove: %v", err)
	}
	got, err = s.GetAgentSubscription("user-1", "googlechat", "deploy", "grove-B")
	if err != nil {
		t.Fatalf("get subB after wrong-grove delete: %v", err)
	}
	if got == nil {
		t.Fatal("subscription should not have been deleted with wrong grove_id")
	}
}

func TestListAgentSubscriptions_GroveScoped(t *testing.T) {
	s := newTestStore(t)

	for _, sub := range []*AgentSubscription{
		{PlatformUserID: "user-1", Platform: "googlechat", AgentID: "deploy", ProjectID: "grove-A"},
		{PlatformUserID: "user-2", Platform: "googlechat", AgentID: "deploy", ProjectID: "grove-B"},
		{PlatformUserID: "user-1", Platform: "googlechat", AgentID: "deploy", ProjectID: "grove-B"},
	} {
		if err := s.SetAgentSubscription(sub); err != nil {
			t.Fatalf("set subscription: %v", err)
		}
	}

	// List for grove-A should only return user-1.
	subs, err := s.ListAgentSubscriptions("deploy", "grove-A")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subscription for grove-A, got %d", len(subs))
	}
	if subs[0].PlatformUserID != "user-1" {
		t.Errorf("expected user-1, got %s", subs[0].PlatformUserID)
	}

	// List for grove-B should return both user-1 and user-2.
	subs, err = s.ListAgentSubscriptions("deploy", "grove-B")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("expected 2 subscriptions for grove-B, got %d", len(subs))
	}
}

func TestMigrateAgentSubscriptionsPK_PreservesData(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Simulate the old schema by creating the table with the old PK.
	s, err := newStoreWithOldSchema(dbPath)
	if err != nil {
		t.Fatalf("creating old-schema store: %v", err)
	}

	// Insert a subscription using old schema.
	_, err = s.db.Exec(
		`INSERT INTO agent_subscriptions (platform_user_id, platform, agent_id, grove_id, activities)
		 VALUES (?, ?, ?, ?, ?)`,
		"user-1", "googlechat", "deploy", "grove-A", "COMPLETED",
	)
	if err != nil {
		t.Fatalf("inserting old-schema row: %v", err)
	}
	s.Close()

	// Re-open with the new migration code.
	s2, err := New(dbPath)
	if err != nil {
		t.Fatalf("re-opening with migration: %v", err)
	}
	defer s2.Close()

	// Verify data was preserved.
	got, err := s2.GetAgentSubscription("user-1", "googlechat", "deploy", "grove-A")
	if err != nil {
		t.Fatalf("get after migration: %v", err)
	}
	if got == nil {
		t.Fatal("subscription lost during migration")
	}
	if got.Activities != "COMPLETED" {
		t.Errorf("activities = %q, want %q", got.Activities, "COMPLETED")
	}

	// Verify that cross-grove now works (old PK would have clobbered).
	if err := s2.SetAgentSubscription(&AgentSubscription{
		PlatformUserID: "user-1",
		Platform:       "googlechat",
		AgentID:        "deploy",
		ProjectID:        "grove-B",
		Activities:     "ERROR",
	}); err != nil {
		t.Fatalf("set cross-grove sub: %v", err)
	}

	gotA, _ := s2.GetAgentSubscription("user-1", "googlechat", "deploy", "grove-A")
	gotB, _ := s2.GetAgentSubscription("user-1", "googlechat", "deploy", "grove-B")
	if gotA == nil || gotB == nil {
		t.Fatal("cross-grove subscriptions should coexist after migration")
	}
}

// newStoreWithOldSchema creates a store with the original (broken) PK schema
// for testing the migration path.
func newStoreWithOldSchema(dbPath string) (*Store, error) {
	db, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS user_mappings (
			platform_user_id TEXT NOT NULL,
			platform         TEXT NOT NULL,
			hub_user_id      TEXT NOT NULL,
			hub_user_email   TEXT NOT NULL,
			registered_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			registered_by    TEXT NOT NULL DEFAULT 'auto',
			PRIMARY KEY (platform_user_id, platform)
		)`,
		`CREATE TABLE IF NOT EXISTS space_links (
			space_id    TEXT NOT NULL,
			platform    TEXT NOT NULL,
			grove_id    TEXT NOT NULL,
			grove_slug  TEXT NOT NULL,
			linked_by   TEXT NOT NULL,
			linked_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			default_agent TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (space_id, platform)
		)`,
		`CREATE TABLE IF NOT EXISTS agent_subscriptions (
			platform_user_id TEXT NOT NULL,
			platform         TEXT NOT NULL,
			agent_id         TEXT NOT NULL,
			grove_id         TEXT NOT NULL,
			activities       TEXT NOT NULL DEFAULT '',
			subscribed_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (platform_user_id, platform, agent_id)
		)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &Store{db: db}, nil
}

// openDB opens a SQLite database without running migrations.
func openDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
