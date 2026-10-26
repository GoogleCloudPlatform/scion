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
	"database/sql"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// pkg/hub's own copies of four web chat store test helpers, for the pkg/hub
// tests outside the web chat store files (handlers, attachments, UTC
// timestamp and thread-group migration tests).
//
// The originals live in the web chat store test files, which move to
// pkg/hub/webchat (ptone/scion#4269). These four build or read a web chat
// store, so they cannot live once in a shared support package: a non-test
// package that imports pkg/hub (today) or pkg/hub/webchat (after the move)
// cannot be imported by that package's own tests (import cycle), and
// helpers in _test.go files cannot be imported by another package. The
// generic database helpers they use live once in internal/hubtestutil.
// Keep each copy in step with its original.

// hubNewTestWebChatStoreV2 is newTestWebChatStoreV2 (webchannel_helpers_test.go):
// a WebChatStore backed by an in-memory SQLite DB.
func hubNewTestWebChatStoreV2(t *testing.T) (WebChatStore, *sql.DB) {
	t.Helper()
	db := openTestMemorySQLite(t, "sqlite")

	store := NewWebChatStore(db, "sqlite")
	require.NoError(t, store.Init())

	return store, db
}

// hubNewTestWebChatStoreWithConversations is
// newTestWebChatStoreWithConversations (webchannel_helpers_test.go): a
// WebChatStore on an in-memory SQLite DB that also has the Ent-managed
// conversations table, created before Init so the migrations see it.
func hubNewTestWebChatStoreWithConversations(t *testing.T) (WebChatStore, *sql.DB) {
	t.Helper()
	db := openTestMemorySQLite(t, "sqlite")

	// Create the conversations table BEFORE Init so migrations can see it.
	_, err := db.Exec(conversationsTableDDL)
	require.NoError(t, err)

	s := NewWebChatStore(db, "sqlite")
	require.NoError(t, s.Init())

	return s, db
}

// hubNewEntWebChatStore is newEntWebChatStore (webchannel_sqlite_helpers_test.go):
// a webchat store on the ent-migrated test store's database, as the hub does
// in production. It returns the WebChatStore interface instead of the
// concrete SQLite store, which pkg/hub cannot name once it moves.
func hubNewEntWebChatStore(t *testing.T) (WebChatStore, store.Store, *sql.DB) {
	t.Helper()
	s := createTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	dbp, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "test store must expose DB()")
	db := dbp.DB()
	wcs := NewWebChatStore(db, "sqlite")
	require.NoError(t, wcs.Init())
	return wcs, s, db
}

// hubGetPrefs is getPrefs (webchannel_store_thread_groups_marker_test.go).
func hubGetPrefs(t *testing.T, wcs WebChatStore, userID string) *WebChatUserPrefs {
	t.Helper()
	p, err := wcs.GetUserPrefs(context.Background(), userID)
	require.NoError(t, err)
	return p
}
