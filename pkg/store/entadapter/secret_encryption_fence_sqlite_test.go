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

package entadapter

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	entsecret "github.com/GoogleCloudPlatform/scion/pkg/ent/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/secret/secretenvelope"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Writer-fence race on SQLite (ptone/scion#1530 §6; P6 obligation 4): N
// concurrent writers keep writing payloads under the old writer key while
// a policy transition moves the fence to a new key. A write that starts
// after the transition has committed must never commit an old-key payload.
//
// Detection: each attempt records whether the transition had already
// returned when the attempt started. Such an attempt that succeeds, or
// whose payload is found in the database at the end, is a violation.

const (
	raceWriters         = 8
	racePreSuccesses    = 2 * raceWriters // successful writes before the transition starts
	racePostAttempts    = 6               // attempts per writer after the transition returned
	raceMaxAttempts     = 2000            // per writer: a safety bound, never reached
	raceTransitionAfter = 30 * time.Second
)

// openRaceSQLite opens a file-backed SQLite database the way the Hub does
// (WAL, one open connection: SQLite pool defaults force MaxOpenConns 1).
func openRaceSQLite(t *testing.T) *ent.Client {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "fence-race.db")
	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, entc.AutoMigrate(context.Background(), client))
	return client
}

func racePolicy(gen int64, writer string) store.EncryptionPolicy {
	return store.EncryptionPolicy{
		Generation:       gen,
		MinReaderVersion: store.EncryptionPolicyMinReaderVersion,
		AllowedWriteKIDs: []string{writer},
		RequiredReadKIDs: []string{"k1", "k2"},
		State:            store.EncryptionPolicyPrepared,
	}
}

// raceValue is a distinct shaped v2 payload under kid for writer w,
// attempt i (filler bytes only; the store never decrypts).
func raceValue(kid string, w, i int) string {
	body := make([]byte, secretenvelope.NonceSize+secretenvelope.TagSize+8)
	copy(body, fmt.Sprintf("w%03di%05d", w, i))
	return secretenvelope.FormatV2Value(kid, body)
}

// raceWrite writes value to the row of writer w (key) through one of the
// fenced write paths, chosen by attempt number.
type raceWrite func(ctx context.Context, cs *CompositeStore, key string, attempt int, value string) error

// fencedRaceWrite cycles through the fenced payload writes.
func fencedRaceWrite(ctx context.Context, cs *CompositeStore, key string, attempt int, value string) error {
	const scope, scopeID = store.ScopeHub, "fence-race"
	switch attempt % 4 {
	case 0:
		cur, err := cs.GetSecret(ctx, key, scope, scopeID)
		if err != nil {
			return err
		}
		cur.EncryptedValue = value
		return cs.UpdateSecret(ctx, cur)
	case 1:
		_, err := cs.UpsertSecret(ctx, &store.Secret{ID: uuid.NewString(), Key: key, EncryptedValue: value, Scope: scope, ScopeID: scopeID})
		return err
	case 2:
		cur, err := cs.GetSecret(ctx, key, scope, scopeID)
		if err != nil {
			return err
		}
		applied, err := cs.UpdateSecretValueIfVersion(ctx, key, scope, scopeID, cur.Version, value)
		if err == nil && !applied {
			return errors.New("value CAS missed with a single writer per row")
		}
		return err
	default:
		cur, err := cs.GetSecret(ctx, key, scope, scopeID)
		if err != nil {
			return err
		}
		applied, err := cs.ReplaceSecretCiphertext(ctx, cur.ID, cur.Version, cur.EncryptedValue, value)
		if err == nil && !applied {
			return errors.New("ciphertext CAS missed with a single writer per row")
		}
		return err
	}
}

// unfencedRaceWrite is the planted defect: the same update with the fence
// skipped (a raw row update, as an unfenced write path would issue).
func unfencedRaceWrite(ctx context.Context, cs *CompositeStore, key string, _ int, value string) error {
	_, err := cs.SecretStore.client.Secret.Update().
		Where(entsecret.KeyEQ(key), entsecret.ScopeEQ(store.ScopeHub), entsecret.ScopeIDEQ("fence-race")).
		SetEncryptedValue(value).
		AddVersion(1).
		Save(ctx)
	return err
}

type raceResult struct {
	attempts       int64 // every write attempt
	preSuccesses   int64 // successful attempts started before the transition returned
	postAttempts   int64 // attempts started after the transition returned
	postRefused    int64 // of those, refused with ErrEncryptionPolicyMismatch
	violations     []string
	persistedStale []string // post-transition payloads found in the database
}

// runFenceRace seeds one k1 row per writer under a k1 policy, runs the
// writers, moves the policy to k2 once racePreSuccesses writes have
// committed, and stops each writer after racePostAttempts attempts that
// started after the transition returned.
func runFenceRace(t *testing.T, write raceWrite) raceResult {
	t.Helper()
	ctx := context.Background()
	client := openRaceSQLite(t)
	cs := NewCompositeStore(client)

	applied, err := cs.UpdateEncryptionPolicy(ctx, 0, racePolicy(1, "k1"))
	require.NoError(t, err)
	require.True(t, applied)
	for w := 0; w < raceWriters; w++ {
		require.NoError(t, cs.CreateSecret(ctx, &store.Secret{ID: uuid.NewString(), Key: fmt.Sprintf("RACE_%02d", w),
			EncryptedValue: raceValue("k1", w, 0), Scope: store.ScopeHub, ScopeID: "fence-race"}))
	}

	var (
		transitioned atomic.Bool
		successes    atomic.Int64
		res          raceResult
		mu           sync.Mutex
		postValues   = map[string]bool{}
		writerErrs   []error
		wg           sync.WaitGroup
	)
	transitionDone := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(raceTransitionAfter)
		for successes.Load() < racePreSuccesses {
			if time.Now().After(deadline) {
				transitionDone <- errors.New("writers never reached the pre-transition count")
				return
			}
			time.Sleep(time.Millisecond)
		}
		ok, err := cs.UpdateEncryptionPolicy(ctx, 1, racePolicy(2, "k2"))
		if err == nil && !ok {
			err = errors.New("policy transition not applied")
		}
		if err == nil {
			transitioned.Store(true)
		}
		transitionDone <- err
	}()

	for w := 0; w < raceWriters; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			key := fmt.Sprintf("RACE_%02d", w)
			post := 0
			for i := 1; i <= raceMaxAttempts && post < racePostAttempts; i++ {
				after := transitioned.Load()
				value := raceValue("k1", w, i)
				err := write(ctx, cs, key, i, value)
				mu.Lock()
				res.attempts++
				switch {
				case after:
					post++
					res.postAttempts++
					postValues[value] = true
					if err == nil {
						res.violations = append(res.violations, fmt.Sprintf("%s attempt %d committed a k1 payload after the transition", key, i))
					} else if errors.Is(err, store.ErrEncryptionPolicyMismatch) {
						res.postRefused++
					} else {
						writerErrs = append(writerErrs, fmt.Errorf("%s attempt %d: %w", key, i, err))
					}
				case err == nil:
					res.preSuccesses++
					successes.Add(1)
				case !errors.Is(err, store.ErrEncryptionPolicyMismatch):
					// Refused before the flag was set is fine (the
					// transition committed while the attempt ran);
					// anything else is a test failure.
					writerErrs = append(writerErrs, fmt.Errorf("%s attempt %d: %w", key, i, err))
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	require.NoError(t, <-transitionDone)
	require.Empty(t, writerErrs)

	p, err := cs.GetEncryptionPolicy(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), p.Generation)
	page, err := cs.ScanSecretPayloads(ctx, "", store.MaxSecretPayloadScan)
	require.NoError(t, err)
	require.Len(t, page, raceWriters, "rows scanned == rows seeded")
	for _, row := range page {
		if postValues[row.EncryptedValue] {
			res.persistedStale = append(res.persistedStale, row.Key)
		}
	}
	return res
}

// TestSecretEncryptionFence_SQLiteWriterRace: no write that starts after
// the transition commits can commit a payload under the replaced writer
// key, on the Hub's SQLite configuration.
func TestSecretEncryptionFence_SQLiteWriterRace(t *testing.T) {
	res := runFenceRace(t, fencedRaceWrite)

	// Input counts: the race ran with writes on both sides of the
	// transition, and every writer finished its post-transition attempts.
	assert.GreaterOrEqual(t, res.preSuccesses, int64(racePreSuccesses))
	assert.Equal(t, int64(raceWriters*racePostAttempts), res.postAttempts)
	assert.Equal(t, res.postAttempts, res.postRefused, "every post-transition k1 write must be refused")
	assert.Empty(t, res.violations)
	assert.Empty(t, res.persistedStale)
}

// TestSecretEncryptionFence_SQLiteWriterRacePlantedDefect runs the same
// race with the fence skipped and requires the detector to report it, so a
// pass of the race above is evidence and not a blind check.
func TestSecretEncryptionFence_SQLiteWriterRacePlantedDefect(t *testing.T) {
	res := runFenceRace(t, unfencedRaceWrite)

	assert.Equal(t, int64(raceWriters*racePostAttempts), res.postAttempts)
	assert.Zero(t, res.postRefused)
	assert.Len(t, res.violations, raceWriters*racePostAttempts, "the unfenced writer must be caught on every post-transition attempt")
	assert.Len(t, res.persistedStale, raceWriters, "each row must end on an unfenced post-transition payload")
}
