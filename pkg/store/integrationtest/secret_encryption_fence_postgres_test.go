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

//go:build integration

package integrationtest

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	enthubencryptionpolicy "github.com/GoogleCloudPlatform/scion/pkg/ent/hubencryptionpolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/secret/secretenvelope"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// blockWindow is how long a write must stay blocked behind a lock before
// the test releases it.
const blockWindow = 300 * time.Millisecond

func fencePolicyPG(gen int64, writer string) store.EncryptionPolicy {
	return store.EncryptionPolicy{
		Generation: gen, MinReaderVersion: store.EncryptionPolicyMinReaderVersion,
		AllowedWriteKIDs: []string{writer}, RequiredReadKIDs: []string{writer},
		State: store.EncryptionPolicyPrepared,
	}
}

func fakeV2(kid string) string {
	return secretenvelope.FormatV2Value(kid, bytes.Repeat([]byte{0x5A}, secretenvelope.NonceSize+secretenvelope.TagSize+4))
}

// TestSecretEncryptionFence_Postgres pins the PostgreSQL locking of the
// encryption policy fence (ptone/scion#1530 §6): fenced payload writes
// take a shared lock on the policy row and transitions an exclusive one,
// so a write never commits against a policy that a concurrent transition
// is replacing, and a transition waits for in-flight writes.
//
// It requires SCION_TEST_POSTGRES_URL (CI: launch-store-postgres-others):
//
//	go test -tags integration ./pkg/store/integrationtest/... -run TestSecretEncryptionFence_Postgres
func TestSecretEncryptionFence_Postgres(t *testing.T) {
	ctx := context.Background()
	dsn := enttest.NewSchemaURL(t) // skips when Postgres is not configured

	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 4})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	cs := entadapter.NewCompositeStore(client)

	applied, err := cs.UpdateEncryptionPolicy(ctx, 0, fencePolicyPG(1, "k2"))
	require.NoError(t, err)
	require.True(t, applied)

	t.Run("TransitionBlocksWriteWhichSeesTheNewPolicy", func(t *testing.T) {
		tx, err := client.Tx(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		// The transition's exclusive lock on the policy row.
		_, err = tx.HubEncryptionPolicy.Query().Where(enthubencryptionpolicy.IDEQ(1)).ForUpdate().Only(ctx)
		require.NoError(t, err)

		done := make(chan error, 1)
		go func() {
			done <- cs.CreateSecret(ctx, &store.Secret{ID: uuid.NewString(), Key: "PG_FENCE_BLOCKED",
				EncryptedValue: fakeV2("k2"), Scope: store.ScopeHub, ScopeID: "pg-fence"})
		}()
		select {
		case err := <-done:
			t.Fatalf("fenced write did not wait for the policy transition: %v", err)
		case <-time.After(blockWindow):
		}
		// Move the fence to k3 and commit: the blocked k2 write must now be
		// refused, not committed under the old policy.
		_, err = tx.HubEncryptionPolicy.UpdateOneID(1).SetGeneration(2).SetAllowedWriteKids([]string{"k3"}).
			SetRequiredReadKids([]string{"k3"}).SetUpdatedAt(time.Now().UTC()).Save(ctx)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		select {
		case err := <-done:
			assert.ErrorIs(t, err, store.ErrEncryptionPolicyMismatch)
		case <-time.After(30 * time.Second):
			t.Fatal("fenced write never completed")
		}
		_, err = cs.GetSecret(ctx, "PG_FENCE_BLOCKED", store.ScopeHub, "pg-fence")
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("OpenWriteBlocksTransition", func(t *testing.T) {
		release := make(chan struct{})
		written := make(chan struct{})
		writeDone := make(chan error, 1)
		go func() {
			writeDone <- cs.WithTx(ctx, func(tx store.Store) error {
				if err := tx.CreateSecret(ctx, &store.Secret{ID: uuid.NewString(), Key: "PG_FENCE_INFLIGHT",
					EncryptedValue: fakeV2("k3"), Scope: store.ScopeHub, ScopeID: "pg-fence"}); err != nil {
					return err
				}
				close(written)
				<-release
				return nil
			})
		}()
		select {
		case <-written:
		case err := <-writeDone:
			t.Fatalf("in-flight write failed: %v", err)
		case <-time.After(30 * time.Second):
			t.Fatal("in-flight write never ran")
		}

		transition := make(chan error, 1)
		go func() {
			ok, err := cs.UpdateEncryptionPolicy(ctx, 2, fencePolicyPG(3, "k4"))
			if err == nil && !ok {
				err = assert.AnError
			}
			transition <- err
		}()
		select {
		case err := <-transition:
			t.Fatalf("policy transition did not wait for the in-flight write: %v", err)
		case <-time.After(blockWindow):
		}
		close(release)
		require.NoError(t, <-writeDone)
		select {
		case err := <-transition:
			require.NoError(t, err)
		case <-time.After(30 * time.Second):
			t.Fatal("policy transition never completed")
		}
		got, err := cs.GetSecret(ctx, "PG_FENCE_INFLIGHT", store.ScopeHub, "pg-fence")
		require.NoError(t, err)
		assert.Equal(t, fakeV2("k3"), got.EncryptedValue)
		p, err := cs.GetEncryptionPolicy(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(3), p.Generation)
	})

	t.Run("RowLockedReplaceBlocksUnchangedValueWriteBack", func(t *testing.T) {
		// A fenced write that re-writes what it read as the stored value
		// must not race a ciphertext replacement: it waits for the row
		// lock, then sees the new bytes, so its (stale) value is a payload
		// change and is fenced, never written back unfenced.
		key, scope, scopeID := "PG_FENCE_REPLACE_RACE", store.ScopeHub, "pg-fence"
		require.NoError(t, cs.CreateSecret(ctx, &store.Secret{ID: uuid.NewString(), Key: key,
			EncryptedValue: fakeV2("k4"), Scope: scope, ScopeID: scopeID}))
		stale, err := cs.GetSecret(ctx, key, scope, scopeID)
		require.NoError(t, err)

		// Move the fence to k5 (policy generation 3 -> 4); the stored k4
		// value is now one a writer may not re-introduce.
		applied, err := cs.UpdateEncryptionPolicy(ctx, 3, fencePolicyPG(4, "k5"))
		require.NoError(t, err)
		require.True(t, applied)

		release := make(chan struct{})
		replaced := make(chan struct{})
		replaceDone := make(chan error, 1)
		go func() {
			replaceDone <- cs.WithTx(ctx, func(tx store.Store) error {
				ok, err := tx.ReplaceSecretCiphertext(ctx, stale.ID, stale.Version, stale.EncryptedValue, fakeV2("k5"))
				if err != nil {
					return err
				}
				if !ok {
					return assert.AnError
				}
				close(replaced)
				<-release
				return nil
			})
		}()
		select {
		case <-replaced:
		case err := <-replaceDone:
			t.Fatalf("replace failed: %v", err)
		case <-time.After(30 * time.Second):
			t.Fatal("replace never ran")
		}

		writeBack := make(chan error, 1)
		go func() {
			upd := *stale
			upd.Description = "metadata write carrying the stale payload"
			writeBack <- cs.UpdateSecret(ctx, &upd)
		}()
		select {
		case err := <-writeBack:
			t.Fatalf("write-back did not wait for the replaced row: %v", err)
		case <-time.After(blockWindow):
		}
		close(release)
		require.NoError(t, <-replaceDone)
		select {
		case err := <-writeBack:
			assert.ErrorIs(t, err, store.ErrEncryptionPolicyMismatch, "the stale payload must be fenced")
		case <-time.After(30 * time.Second):
			t.Fatal("write-back never completed")
		}
		got, err := cs.GetSecret(ctx, key, scope, scopeID)
		require.NoError(t, err)
		assert.Equal(t, fakeV2("k5"), got.EncryptedValue, "the replacement must survive")
	})
}

// TestSecretEncryptionPolicyCreation_Postgres pins that creating the policy
// (no row yet) waits for an in-flight fenced write on PostgreSQL (the
// transition's table lock conflicts with the writer's share lock), that a
// write after the creation commits sees the policy, and that a second
// creator gets applied=false. TestSecretEncryptionPolicyActivation_Postgres
// covers a writer that arrives during the creation.
func TestSecretEncryptionPolicyCreation_Postgres(t *testing.T) {
	ctx := context.Background()
	dsn := enttest.NewSchemaURL(t) // skips when Postgres is not configured
	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 4})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	cs := entadapter.NewCompositeStore(client)

	// An in-flight plaintext write (allowed: no policy yet) holds the
	// creation back until it commits.
	release := make(chan struct{})
	written := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- cs.WithTx(ctx, func(tx store.Store) error {
			if err := tx.CreateSecret(ctx, &store.Secret{ID: uuid.NewString(), Key: "PG_CREATE_INFLIGHT",
				EncryptedValue: "fake-plain", Scope: store.ScopeHub, ScopeID: "pg-create"}); err != nil {
				return err
			}
			close(written)
			<-release
			return nil
		})
	}()
	select {
	case <-written:
	case err := <-writeDone:
		t.Fatalf("in-flight write failed: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("in-flight write never ran")
	}
	created := make(chan error, 1)
	go func() {
		ok, err := cs.UpdateEncryptionPolicy(ctx, 0, fencePolicyPG(1, "k2"))
		if err == nil && !ok {
			err = assert.AnError
		}
		created <- err
	}()
	select {
	case err := <-created:
		t.Fatalf("policy creation did not wait for the in-flight write: %v", err)
	case <-time.After(blockWindow):
	}
	close(release)
	require.NoError(t, <-writeDone)
	select {
	case err := <-created:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("policy creation never completed")
	}

	// A write that starts after the creation sees the new policy.
	err = cs.CreateSecret(ctx, &store.Secret{ID: uuid.NewString(), Key: "PG_CREATE_AFTER",
		EncryptedValue: "fake-plain", Scope: store.ScopeHub, ScopeID: "pg-create"})
	assert.ErrorIs(t, err, store.ErrEncryptionPolicyMismatch)

	// A second creator gets applied=false, not an error.
	ok, err := cs.UpdateEncryptionPolicy(ctx, 0, fencePolicyPG(1, "k3"))
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestSecretEncryptionPolicyActivation_Postgres: a fenced write that
// arrives while a real policy creation (UpdateEncryptionPolicy 0 -> 1) is
// uncommitted waits for it (the creation holds LOCK TABLE ... IN EXCLUSIVE
// MODE, which the writer's FOR SHARE policy read conflicts with even with
// no row), then sees the new policy and is refused. Without the table lock
// the writer would find no policy row and commit plaintext.
func TestSecretEncryptionPolicyActivation_Postgres(t *testing.T) {
	ctx := context.Background()
	dsn := enttest.NewSchemaURL(t) // skips when Postgres is not configured
	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 4})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	cs := entadapter.NewCompositeStore(client)

	release := make(chan struct{})
	activated := make(chan struct{})
	activationDone := make(chan error, 1)
	go func() {
		activationDone <- cs.WithTx(ctx, func(tx store.Store) error {
			ok, err := tx.UpdateEncryptionPolicy(ctx, 0, fencePolicyPG(1, "k2"))
			if err != nil {
				return err
			}
			if !ok {
				return assert.AnError
			}
			close(activated)
			<-release
			return nil
		})
	}()
	select {
	case <-activated:
	case err := <-activationDone:
		t.Fatalf("activation failed: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("activation never ran")
	}

	write := make(chan error, 1)
	go func() {
		write <- cs.CreateSecret(ctx, &store.Secret{ID: uuid.NewString(), Key: "PG_ACTIVATION_RACE",
			EncryptedValue: "fake-plain", Scope: store.ScopeHub, ScopeID: "pg-activate"})
	}()
	select {
	case err := <-write:
		t.Fatalf("fenced write did not wait for the uncommitted policy creation: %v", err)
	case <-time.After(blockWindow):
	}
	close(release)
	require.NoError(t, <-activationDone)
	select {
	case err := <-write:
		assert.ErrorIs(t, err, store.ErrEncryptionPolicyMismatch, "the write must see the new policy")
	case <-time.After(30 * time.Second):
		t.Fatal("fenced write never completed")
	}
	_, err = cs.GetSecret(ctx, "PG_ACTIVATION_RACE", store.ScopeHub, "pg-activate")
	assert.ErrorIs(t, err, store.ErrNotFound, "no plaintext may be committed past the activation")
}
