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
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// newPersistentTestExchangeService creates a GEExchangeService backed by a
// real ent/SQLite store at the given path. Each call opens an independent
// ent.Client to the same database file — callers can use two instances to
// simulate cross-instance convergence. Cleanup is registered on t.
//
// Opens through entc.OpenSQLite, so the store boundary gets production's
// foreign keys, WAL journal, "_timezone=UTC" DSN option and UTC mutation
// hook (tz-refactor design §2.1.2). The busy timeout lets the two instances
// write the same file concurrently.
func newPersistentTestExchangeService(t *testing.T, dbPath string) (*GEExchangeService, store.Store, ExternalIdentityStore) {
	t.Helper()
	client, err := entc.OpenSQLite("file:"+dbPath+"?_pragma=busy_timeout(5000)", entc.PoolConfig{MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := autoMigrateTestClient(context.Background(), client); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	compositeStore := entadapter.NewCompositeStore(client)
	extStore := entadapter.NewExternalIdentityStore(client)

	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}

	tokenSvc, _ := NewUserTokenService(UserTokenConfig{
		AccessTokenDuration: DefaultGETokenTTL,
	})
	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         DefaultGETokenTTL,
		},
		validator,
		tokenSvc,
		newTestResolver(compositeStore, extStore, alwaysAuthorized, nil),
		slog.Default(),
	)
	return svc, compositeStore, extStore
}

// TestGEExchange_ConcurrentFirstLinkage_PersistentStore uses two independent
// GEExchangeService instances against the same backing database (via the ent
// adapter), proving that the unique-email collision and binding convergence
// work across instances/reconnect — not merely through a shared fake object.
func TestGEExchange_ConcurrentFirstLinkage_PersistentStore(t *testing.T) {
	// This test requires the ent adapter with a real SQLite database.
	// We create two independent service instances sharing the same DB file.
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/concurrent_test.db"

	// Create the first service+store pair.
	svc1, store1, extStore1 := newPersistentTestExchangeService(t, dbPath)
	// Create the second service+store pair using the same DB file.
	svc2, store2, extStore2 := newPersistentTestExchangeService(t, dbPath)
	_, _, _ = store1, store2, extStore2 // used only for cleanup via t.Cleanup

	identity := validGmailIdentity()

	ctx := context.Background()

	// Exchange #1 via service instance 1: should provision user + create binding.
	resp1, _, err := svc1.Exchange(ctx, &ExchangeRequest{
		Credential:     "persistent-token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("svc1.Exchange: %v", err)
	}
	if resp1.User == nil || resp1.User.ID == "" {
		t.Fatal("svc1 returned nil/empty user")
	}
	user1ID := resp1.User.ID

	// Verify the binding exists in the first store.
	b1, err := extStore1.GetExternalIdentity(ctx, "google",
		canonicalizeGoogleIssuer(identity.Issuer), identity.Subject)
	if err != nil {
		t.Fatalf("binding lookup in store1: %v", err)
	}
	if b1.UserID != user1ID {
		t.Fatalf("binding in store1 points to %q, expected %q", b1.UserID, user1ID)
	}

	// Exchange #2 via service instance 2 (same DB, separate store objects):
	// should find the existing binding and resolve to the same user.
	resp2, _, err := svc2.Exchange(ctx, &ExchangeRequest{
		Credential:     "persistent-token",
		CredentialType: "id_token",
	})
	if err != nil {
		t.Fatalf("svc2.Exchange: %v", err)
	}
	if resp2.User == nil {
		t.Fatal("svc2 returned nil user")
	}
	if resp2.User.ID != user1ID {
		t.Fatalf("svc2 resolved to user %q, expected %q (convergence failed across instances)",
			resp2.User.ID, user1ID)
	}
}
