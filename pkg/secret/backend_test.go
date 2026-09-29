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

package secret

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ============================================================================
// Resolve: nil AuthzCheck excludes progeny secrets (fail closed by default)
// ============================================================================

// TestResolve_NilAuthzCheckExcludesSharedSecrets verifies that when Resolve
// is called with a non-empty AgentAncestry and no AuthzCheck callback,
// progeny secrets are excluded rather than included. A caller must supply an
// explicit policy decision before any progeny value is read; a missing
// checker is not an implicit allow. This holds for both backends.
func TestResolve_NilAuthzCheckExcludesSharedSecrets(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		backend, s := createTestBackend(t)
		ctx := context.Background()

		seedSecret(t, s, &store.Secret{
			ID:             tid("resolve-nil-authz-local"),
			Key:            "NO_AUTHZ_KEY",
			EncryptedValue: "no-authz-value",
			SecretType:     store.SecretTypeEnvironment,
			Target:         "NO_AUTHZ_KEY",
			Scope:          store.ScopeUser,
			ScopeID:        "alice-123",
			AllowProgeny:   true,
			CreatedBy:      "alice-123",
		})

		opts := &ResolveOpts{
			AgentAncestry: []string{"alice-123", "agent-a"},
			AuthzCheck:    nil,
		}
		resolved, err := backend.Resolve(ctx, "", "", "", opts)
		if err != nil {
			t.Fatalf("Resolve failed: %v", err)
		}
		for _, sv := range resolved {
			if sv.Name == "NO_AUTHZ_KEY" {
				t.Error("progeny secret should be excluded when AuthzCheck is nil")
			}
		}
	})

	t.Run("gcp", func(t *testing.T) {
		backend, _ := createTestGCPBackend(t)
		ctx := context.Background()

		_, _, err := backend.Set(ctx, &SetSecretInput{
			Name:         "NO_AUTHZ_KEY",
			Value:        "no-authz-value",
			SecretType:   TypeEnvironment,
			Scope:        ScopeUser,
			ScopeID:      "alice-123",
			AllowProgeny: true,
			CreatedBy:    "alice-123",
		})
		if err != nil {
			t.Fatalf("Set failed: %v", err)
		}

		opts := &ResolveOpts{
			AgentAncestry: []string{"alice-123", "agent-a"},
			AuthzCheck:    nil,
		}
		resolved, err := backend.Resolve(ctx, "", "", "", opts)
		if err != nil {
			t.Fatalf("Resolve failed: %v", err)
		}
		for _, sv := range resolved {
			if sv.Name == "NO_AUTHZ_KEY" {
				t.Error("progeny secret should be excluded when AuthzCheck is nil")
			}
		}
	})
}

// ============================================================================
// FetchValues: decrypt/access failures and no name-based fallback
// ============================================================================

// TestSecretFetch_DecryptErrorReportsUnavailable verifies that a per-item
// decrypt (local) or Secret Manager access (GCP) failure is reported as a
// FetchResult error, never as an empty value delivered in place of an error.
func TestSecretFetch_DecryptErrorReportsUnavailable(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		backend, s := createTestBackend(t)
		ctx := context.Background()

		// enc:v1: prefixed, valid base64, but not a value EncryptValue ever
		// produced for this key: it decodes but fails AES-GCM authentication.
		corrupt := EncryptedPrefix + base64.StdEncoding.EncodeToString(make([]byte, 32))
		seedSecret(t, s, &store.Secret{
			ID:             tid("fetch-decrypt-fail"),
			Key:            "CORRUPT_KEY",
			EncryptedValue: corrupt,
			SecretType:     store.SecretTypeEnvironment,
			Target:         "CORRUPT_KEY",
			Scope:          store.ScopeUser,
			ScopeID:        "user-1",
		})
		meta, err := backend.GetMeta(ctx, "CORRUPT_KEY", ScopeUser, "user-1")
		if err != nil {
			t.Fatalf("GetMeta failed: %v", err)
		}

		results, err := backend.FetchValues(ctx, []SecretMeta{*meta})
		if err != nil {
			t.Fatalf("FetchValues failed: %v", err)
		}
		res, ok := results[meta.ID]
		if !ok {
			t.Fatal("expected a result keyed by the requested ID")
		}
		if res.Err == nil {
			t.Fatal("expected a decrypt error, got nil")
		}
		if res.Value != "" {
			t.Errorf("expected empty value on decrypt failure, got %q", res.Value)
		}
	})

	t.Run("gcp", func(t *testing.T) {
		backend, mock := createTestGCPBackend(t)
		ctx := context.Background()

		_, meta, err := backend.Set(ctx, &SetSecretInput{
			Name:       "ACCESS_FAIL_KEY",
			Value:      "value",
			SecretType: TypeEnvironment,
			Scope:      ScopeProject,
			ScopeID:    "project-1",
		})
		if err != nil {
			t.Fatalf("Set failed: %v", err)
		}

		// The Hub database record survives, but the Secret Manager version
		// becomes unreadable (e.g. the secret was destroyed out of band).
		smPath, ok := extractGCPSMPath(meta.SecretRef)
		if !ok {
			t.Fatalf("expected a gcpsm SecretRef, got %q", meta.SecretRef)
		}
		mock.mu.Lock()
		delete(mock.versions, smPath)
		mock.mu.Unlock()

		results, err := backend.FetchValues(ctx, []SecretMeta{*meta})
		if err != nil {
			t.Fatalf("FetchValues failed: %v", err)
		}
		res := results[meta.ID]
		if res.Err == nil {
			t.Fatal("expected an access error, got nil")
		}
		if res.Value != "" {
			t.Errorf("expected empty value on access failure, got %q", res.Value)
		}
	})
}

// TestSecretFetch_MissingRecordNotResolvedByName verifies that FetchValues
// never falls back to a name-based lookup: unlike Get (which recovers a
// value from GCP Secret Manager by computed name when the Hub database
// record is missing, to survive a database reset), FetchValues reports
// store.ErrNotFound for a record that isn't in the database, even when a
// value reachable by the same computed name exists elsewhere.
func TestSecretFetch_MissingRecordNotResolvedByName(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		backend, _ := createTestBackend(t)
		ctx := context.Background()

		meta := SecretMeta{ID: tid("local-missing"), Name: "GHOST_KEY", Scope: ScopeUser, ScopeID: "user-1", Version: 1}
		results, err := backend.FetchValues(ctx, []SecretMeta{meta})
		if err != nil {
			t.Fatalf("FetchValues failed: %v", err)
		}
		if res := results[meta.ID]; res.Err != store.ErrNotFound {
			t.Errorf("expected store.ErrNotFound, got value=%q err=%v", res.Value, res.Err)
		}
	})

	t.Run("gcp", func(t *testing.T) {
		backend, mock := createTestGCPBackend(t)
		ctx := context.Background()

		// Simulate a database reset: a value is reachable in Secret Manager
		// under the computed name, but there is no Hub database record.
		smName := backend.gcpSecretName("GHOST_KEY", ScopeHub, "test-hub-id")
		fullName := fmt.Sprintf("projects/%s/secrets/%s", backend.projectID, smName)
		mock.mu.Lock()
		mock.secrets[fullName] = &smpb.Secret{Name: fullName}
		mock.versions[fullName] = []byte("recoverable-by-name-value")
		mock.mu.Unlock()

		// Sanity check: Get recovers it. This is existing, unchanged
		// behaviour reserved for Get's hub-internal callers (see its doc
		// comment), not a fallback FetchValues shares.
		sv, err := backend.Get(ctx, "GHOST_KEY", ScopeHub, "test-hub-id")
		if err != nil {
			t.Fatalf("Get should recover the value from GCP SM by computed name: %v", err)
		}
		if sv.Value != "recoverable-by-name-value" {
			t.Fatalf("unexpected recovered value: %q", sv.Value)
		}

		// FetchValues must not: with no DB record, it never reaches the
		// computed-name fallback.
		meta := SecretMeta{ID: tid("gcp-missing"), Name: "GHOST_KEY", Scope: ScopeHub, ScopeID: "test-hub-id", Version: 1}
		results, err := backend.FetchValues(ctx, []SecretMeta{meta})
		if err != nil {
			t.Fatalf("FetchValues failed: %v", err)
		}
		if res := results[meta.ID]; res.Err != store.ErrNotFound {
			t.Errorf("expected store.ErrNotFound (no name-based fallback), got value=%q err=%v", res.Value, res.Err)
		}
	})
}

// ============================================================================
// FetchValues: record-generation check
// ============================================================================

// TestSecretFetch_RecordGenerationChecked verifies that FetchValues rejects a
// meta whose recorded generation no longer matches the current record: a
// secret deleted and recreated under the same name/scope, a rotated
// version, a reclassified (internal) type, a meta whose ID doesn't match the
// record found at its own triple, and a same-named record living in a
// different scope. A positive control confirms the current meta still
// resolves.
//
// Each negative subtest reaches the record comparison itself: the record
// exists at the meta's triple, so GetSecret succeeds and only the
// comparison can reject it.
func TestSecretFetch_RecordGenerationChecked(t *testing.T) {
	run := func(t *testing.T, backend SecretBackend) {
		ctx := context.Background()

		checkNotFound := func(t *testing.T, meta SecretMeta) {
			t.Helper()
			results, err := backend.FetchValues(ctx, []SecretMeta{meta})
			if err != nil {
				t.Fatalf("FetchValues failed: %v", err)
			}
			res := results[meta.ID]
			if res.Err != store.ErrNotFound {
				t.Errorf("expected store.ErrNotFound, got value=%q err=%v", res.Value, res.Err)
			}
			if res.Value != "" {
				t.Errorf("expected empty value, got %q", res.Value)
			}
		}

		t.Run("recreated", func(t *testing.T) {
			_, oldMeta, err := backend.Set(ctx, &SetSecretInput{
				Name: "RECREATE_KEY", Value: "v1", SecretType: TypeEnvironment,
				Scope: ScopeUser, ScopeID: "recreate-user",
			})
			if err != nil {
				t.Fatalf("Set failed: %v", err)
			}
			if err := backend.Delete(ctx, "RECREATE_KEY", ScopeUser, "recreate-user"); err != nil {
				t.Fatalf("Delete failed: %v", err)
			}
			if _, _, err := backend.Set(ctx, &SetSecretInput{
				Name: "RECREATE_KEY", Value: "v2", SecretType: TypeEnvironment,
				Scope: ScopeUser, ScopeID: "recreate-user",
			}); err != nil {
				t.Fatalf("Set (recreate) failed: %v", err)
			}
			// oldMeta's ID no longer identifies any record at this triple.
			checkNotFound(t, *oldMeta)
		})

		t.Run("rotated", func(t *testing.T) {
			_, meta1, err := backend.Set(ctx, &SetSecretInput{
				Name: "ROTATE_KEY", Value: "v1", SecretType: TypeEnvironment,
				Scope: ScopeUser, ScopeID: "rotate-user",
			})
			if err != nil {
				t.Fatalf("Set failed: %v", err)
			}
			if _, _, err := backend.Set(ctx, &SetSecretInput{
				Name: "ROTATE_KEY", Value: "v2", SecretType: TypeEnvironment,
				Scope: ScopeUser, ScopeID: "rotate-user",
			}); err != nil {
				t.Fatalf("Set (rotate) failed: %v", err)
			}
			// meta1 is the pre-rotation Version; the record has since moved on.
			checkNotFound(t, *meta1)
		})

		t.Run("reclassified", func(t *testing.T) {
			if _, _, err := backend.Set(ctx, &SetSecretInput{
				Name: "RECLASS_KEY", Value: "v1", SecretType: TypeEnvironment,
				Scope: ScopeUser, ScopeID: "reclass-user",
			}); err != nil {
				t.Fatalf("Set failed: %v", err)
			}
			refreshed, err := backend.UpdateMeta(ctx, &UpdateMetaInput{
				Name: "RECLASS_KEY", Scope: ScopeUser, ScopeID: "reclass-user",
				SecretType: store.SecretTypeInternal, UpdatedBy: "tester",
			})
			if err != nil {
				t.Fatalf("UpdateMeta failed: %v", err)
			}
			// refreshed already reflects the internal reclassification.
			checkNotFound(t, *refreshed)
		})

		t.Run("id_mismatch", func(t *testing.T) {
			_, meta, err := backend.Set(ctx, &SetSecretInput{
				Name: "IDMISMATCH_KEY", Value: "v1", SecretType: TypeEnvironment,
				Scope: ScopeUser, ScopeID: "idmismatch-user",
			})
			if err != nil {
				t.Fatalf("Set failed: %v", err)
			}
			bad := *meta
			bad.ID = tid("record-generation-wrong-id")
			checkNotFound(t, bad)
		})

		t.Run("cross_scope", func(t *testing.T) {
			_, metaA, err := backend.Set(ctx, &SetSecretInput{
				Name: "SHARED_NAME", Value: "a", SecretType: TypeEnvironment,
				Scope: ScopeUser, ScopeID: "cross-scope-a",
			})
			if err != nil {
				t.Fatalf("Set (scope A) failed: %v", err)
			}
			_, metaB, err := backend.Set(ctx, &SetSecretInput{
				Name: "SHARED_NAME", Value: "b", SecretType: TypeEnvironment,
				Scope: ScopeUser, ScopeID: "cross-scope-b",
			})
			if err != nil {
				t.Fatalf("Set (scope B) failed: %v", err)
			}
			// The caller's triple names scope B, but its ID is scope A's record.
			mixed := *metaB
			mixed.ID = metaA.ID
			checkNotFound(t, mixed)
		})

		t.Run("positive_control", func(t *testing.T) {
			_, meta, err := backend.Set(ctx, &SetSecretInput{
				Name: "GOOD_KEY", Value: "good-value", SecretType: TypeEnvironment,
				Scope: ScopeUser, ScopeID: "control-user",
			})
			if err != nil {
				t.Fatalf("Set failed: %v", err)
			}
			results, err := backend.FetchValues(ctx, []SecretMeta{*meta})
			if err != nil {
				t.Fatalf("FetchValues failed: %v", err)
			}
			res := results[meta.ID]
			if res.Err != nil || res.Value != "good-value" {
				t.Errorf("expected value=%q err=nil, got value=%q err=%v", "good-value", res.Value, res.Err)
			}
		})
	}

	t.Run("local", func(t *testing.T) {
		backend, _ := createTestBackend(t)
		run(t, backend)
	})
	t.Run("gcp", func(t *testing.T) {
		backend, _ := createTestGCPBackend(t)
		run(t, backend)
	})
}

// TestRecordGenerationChanged_NilRecordFailsClosed verifies that
// recordGenerationChanged reports a change, rather than dereferencing a nil
// record, when its store.Secret argument is nil. No current caller passes
// recordGenerationChanged a nil record — each only reaches the call after
// SecretStore.GetSecret has returned a nil error, and that interface's
// contract promises a non-nil record in that case — but the guard is
// exercised directly here as defense in depth against a future
// implementation that violates the contract.
func TestRecordGenerationChanged_NilRecordFailsClosed(t *testing.T) {
	meta := SecretMeta{ID: tid("nil-record-meta"), Version: 1}
	if !recordGenerationChanged(nil, meta) {
		t.Error("expected recordGenerationChanged(nil, meta) to report changed (fail closed), got false")
	}
}

// TestSecretFetch_MetaFieldChangedAtSameVersionNotDelivered verifies that
// FetchValues also compares AllowProgeny, CreatedBy and SecretType, not
// just ID and Version. UpdateSecretMeta is a read-modify-write with no
// version predicate, so two concurrent metadata updates can each bump
// Version from the same baseline and land on the same new Version with
// different field values; Version alone would not catch that. Each subtest
// simulates the caller having captured a meta whose recorded field disagrees
// with the current record even though the Version matches, by copying the
// live meta and changing exactly one field.
func TestSecretFetch_MetaFieldChangedAtSameVersionNotDelivered(t *testing.T) {
	run := func(t *testing.T, backend SecretBackend) {
		ctx := context.Background()

		checkNotFound := func(t *testing.T, meta SecretMeta) {
			t.Helper()
			results, err := backend.FetchValues(ctx, []SecretMeta{meta})
			if err != nil {
				t.Fatalf("FetchValues failed: %v", err)
			}
			res := results[meta.ID]
			if res.Err != store.ErrNotFound {
				t.Errorf("expected store.ErrNotFound, got value=%q err=%v", res.Value, res.Err)
			}
			if res.Value != "" {
				t.Errorf("expected empty value, got %q", res.Value)
			}
		}

		t.Run("allow_progeny_changed", func(t *testing.T) {
			_, meta, err := backend.Set(ctx, &SetSecretInput{
				Name: "AP_RACE_KEY", Value: "v1", SecretType: TypeEnvironment,
				Scope: ScopeUser, ScopeID: "ap-race-user", AllowProgeny: true, CreatedBy: "ap-race-user",
			})
			if err != nil {
				t.Fatalf("Set failed: %v", err)
			}
			stale := *meta
			stale.AllowProgeny = false // disagrees with the live record, same Version
			checkNotFound(t, stale)
		})

		t.Run("created_by_changed", func(t *testing.T) {
			_, meta, err := backend.Set(ctx, &SetSecretInput{
				Name: "CB_RACE_KEY", Value: "v1", SecretType: TypeEnvironment,
				Scope: ScopeUser, ScopeID: "cb-race-user", CreatedBy: "original-creator",
			})
			if err != nil {
				t.Fatalf("Set failed: %v", err)
			}
			stale := *meta
			stale.CreatedBy = "different-creator" // disagrees with the live record, same Version
			checkNotFound(t, stale)
		})

		t.Run("secret_type_changed", func(t *testing.T) {
			_, meta, err := backend.Set(ctx, &SetSecretInput{
				Name: "ST_RACE_KEY", Value: "v1", SecretType: TypeEnvironment,
				Scope: ScopeUser, ScopeID: "st-race-user",
			})
			if err != nil {
				t.Fatalf("Set failed: %v", err)
			}
			stale := *meta
			stale.SecretType = TypeFile // disagrees with the live record, same Version
			checkNotFound(t, stale)
		})
	}

	t.Run("local", func(t *testing.T) {
		backend, _ := createTestBackend(t)
		run(t, backend)
	})
	t.Run("gcp", func(t *testing.T) {
		backend, _ := createTestGCPBackend(t)
		run(t, backend)
	})
}

// ============================================================================
// FetchValues: GCP post-read re-check
// ============================================================================

// hookSMClient wraps mockSMClient so a test can run code at the exact point
// GCPBackend.fetchValue reads Secret Manager: to place a write around that read,
// or to fail if the read happens at all. Each hook fires at most once and then
// clears itself.
type hookSMClient struct {
	*mockSMClient
	beforeAccess, afterAccess func()
}

func (h *hookSMClient) AccessSecretVersion(ctx context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	if f := h.beforeAccess; f != nil {
		h.beforeAccess = nil
		f()
	}
	resp, err := h.mockSMClient.AccessSecretVersion(ctx, req)
	if f := h.afterAccess; f != nil {
		h.afterAccess = nil
		f()
	}
	return resp, err
}

// TestSecretFetch_RecordChangedDuringSecretManagerReadNotDelivered verifies
// that GCPBackend.fetchValue's post-read re-check catches a database write
// landing between the Secret Manager read and that re-read, instead of
// delivering the Secret Manager value it just read under metadata that no
// longer matches the current record. It models GCPBackend.Set's own write
// order: the Secret Manager version changes before the database record does.
func TestSecretFetch_RecordChangedDuringSecretManagerReadNotDelivered(t *testing.T) {
	ctx := context.Background()
	st, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	hook := &hookSMClient{mockSMClient: newMockSMClient()}
	backend := NewGCPBackendWithClient(st, hook, "test-project", "test-hub-id")

	_, meta, err := backend.Set(ctx, &SetSecretInput{
		Name: "RACE_KEY", Value: "v1", SecretType: TypeEnvironment,
		Scope: ScopeUser, ScopeID: "race-user",
	})
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	smPath, ok := extractGCPSMPath(meta.SecretRef)
	if !ok {
		t.Fatalf("meta.SecretRef %q is not a gcpsm ref", meta.SecretRef)
	}

	// beforeAccess lands the new Secret Manager version immediately before
	// fetchValue's read of it, so that read returns the new value.
	hook.beforeAccess = func() {
		hook.mockSMClient.mu.Lock()
		hook.mockSMClient.versions[smPath] = []byte("v2")
		hook.mockSMClient.mu.Unlock()
	}
	// afterAccess lands the corresponding database write right after
	// fetchValue's Secret Manager read but before its re-read, the same way
	// a concurrent Set's own database upsert would.
	hook.afterAccess = func() {
		current, err := backend.store.GetSecret(ctx, meta.Name, meta.Scope, meta.ScopeID)
		if err != nil {
			t.Fatalf("GetSecret failed: %v", err)
		}
		if _, err := backend.store.UpsertSecret(ctx, current); err != nil {
			t.Fatalf("UpsertSecret failed: %v", err)
		}
	}

	results, err := backend.FetchValues(ctx, []SecretMeta{*meta})
	if err != nil {
		t.Fatalf("FetchValues failed: %v", err)
	}
	res := results[meta.ID]
	if res.Err != store.ErrNotFound {
		t.Errorf("expected store.ErrNotFound, got value=%q err=%v", res.Value, res.Err)
	}
	if res.Value != "" {
		t.Errorf("expected empty value, got %q", res.Value)
	}
}

// TestSecretFetch_MismatchedRecordNotReadFromSecretManager verifies that
// GCPBackend.fetchValue rejects a meta whose record no longer matches
// before it reads Secret Manager, not only in the post-read re-check: a
// stale meta never causes a Secret Manager access.
func TestSecretFetch_MismatchedRecordNotReadFromSecretManager(t *testing.T) {
	ctx := context.Background()
	st, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	hook := &hookSMClient{mockSMClient: newMockSMClient()}
	backend := NewGCPBackendWithClient(st, hook, "test-project", "test-hub-id")

	_, oldMeta, err := backend.Set(ctx, &SetSecretInput{
		Name: "PRECHECK_KEY", Value: "v1", SecretType: TypeEnvironment,
		Scope: ScopeUser, ScopeID: "precheck-user",
	})
	if err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if _, _, err := backend.Set(ctx, &SetSecretInput{
		Name: "PRECHECK_KEY", Value: "v2", SecretType: TypeEnvironment,
		Scope: ScopeUser, ScopeID: "precheck-user",
	}); err != nil {
		t.Fatalf("Set (rotate) failed: %v", err)
	}

	hook.beforeAccess = func() {
		t.Error("Secret Manager was read for a record that no longer matches the metadata")
	}
	results, err := backend.FetchValues(ctx, []SecretMeta{*oldMeta})
	if err != nil {
		t.Fatalf("FetchValues failed: %v", err)
	}
	if res := results[oldMeta.ID]; res.Err != store.ErrNotFound || res.Value != "" {
		t.Errorf("expected store.ErrNotFound and empty value, got value=%q err=%v", res.Value, res.Err)
	}
}

// ============================================================================
// Resolve: decrypt failures are skipped
// ============================================================================

// TestResolve_DecryptErrorSkipsSecret verifies that decryptRawValue's
// fail-closed behaviour reaches Resolve: a secret whose stored value fails
// to decrypt is left out of the Resolve result entirely, not delivered with
// an empty value. This covers the Resolve -> decryptRawValue path;
// TestSecretFetch_DecryptErrorReportsUnavailable covers the FetchValues ->
// decryptStoreSecret path.
// Reverting localbackend.go's decryptRawValue to `return "", nil` on a
// decrypt failure turns this test red: the corrupt secret would then be
// merged into the Resolve result with Value == "" instead of being skipped.
func TestResolve_DecryptErrorSkipsSecret(t *testing.T) {
	backend, s := createTestBackend(t)
	ctx := context.Background()

	// enc:v1: prefixed, valid base64, but not a value EncryptValue ever
	// produced for this key: it decodes but fails AES-GCM authentication.
	corrupt := EncryptedPrefix + base64.StdEncoding.EncodeToString(make([]byte, 32))

	seedSecret(t, s, &store.Secret{
		ID:             tid("resolve-decrypt-fail-own"),
		Key:            "CORRUPT_OWN_KEY",
		EncryptedValue: corrupt,
		SecretType:     store.SecretTypeEnvironment,
		Target:         "CORRUPT_OWN_KEY",
		Scope:          store.ScopeProject,
		ScopeID:        "project-1",
	})
	seedSecret(t, s, &store.Secret{
		ID:             tid("resolve-decrypt-fail-progeny"),
		Key:            "CORRUPT_PROGENY_KEY",
		EncryptedValue: corrupt,
		SecretType:     store.SecretTypeEnvironment,
		Target:         "CORRUPT_PROGENY_KEY",
		Scope:          store.ScopeUser,
		ScopeID:        "alice-123",
		AllowProgeny:   true,
		CreatedBy:      "alice-123",
	})

	opts := &ResolveOpts{
		AgentAncestry: []string{"alice-123", "agent-a"},
		AuthzCheck:    func(_ SecretMeta) bool { return true },
	}
	resolved, err := backend.Resolve(ctx, "", "project-1", "", opts)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	for _, sv := range resolved {
		if sv.Name == "CORRUPT_OWN_KEY" {
			t.Error("own-scope secret that fails to decrypt should be absent from Resolve, not present with an empty value")
		}
		if sv.Name == "CORRUPT_PROGENY_KEY" {
			t.Error("progeny secret that fails to decrypt should be absent from Resolve, not present with an empty value")
		}
	}
}

// ============================================================================
// Backend parity
// ============================================================================

// TestMaterialSelection_BackendParity verifies that LocalBackend and
// GCPBackend (backed by a fake Secret Manager client) behave equivalently
// for the same fixtures: own-scope and progeny secrets resolve to the same
// values and types across environment, file and variable secret types, and
// FetchValues reports the same own-scope value and the same
// store.ErrNotFound for a record that was never created.
func TestMaterialSelection_BackendParity(t *testing.T) {
	type seedFunc func(name, scope, scopeID, value, secretType string, allowProgeny bool, createdBy string) SecretMeta

	run := func(t *testing.T, backend SecretBackend, seed seedFunc) {
		ctx := context.Background()

		ownMeta := seed("OWN_KEY", ScopeProject, "project-1", "own-value", TypeEnvironment, false, "")
		seed("PROGENY_KEY", ScopeUser, "alice-1", "progeny-value", TypeEnvironment, true, "alice-1")
		seed("FILE_KEY", ScopeProject, "project-1", "file-value", TypeFile, false, "")
		seed("VAR_KEY", ScopeProject, "project-1", "var-value", TypeVariable, false, "")

		opts := &ResolveOpts{
			AgentAncestry: []string{"alice-1", "agent-a"},
			AuthzCheck:    func(_ SecretMeta) bool { return true },
		}
		resolved, err := backend.Resolve(ctx, "", "project-1", "", opts)
		if err != nil {
			t.Fatalf("Resolve failed: %v", err)
		}
		byName := make(map[string]SecretWithValue, len(resolved))
		for _, sv := range resolved {
			byName[sv.Name] = sv
		}
		for _, want := range []struct{ name, value, typ string }{
			{"OWN_KEY", "own-value", TypeEnvironment},
			{"PROGENY_KEY", "progeny-value", TypeEnvironment},
			{"FILE_KEY", "file-value", TypeFile},
			{"VAR_KEY", "var-value", TypeVariable},
		} {
			sv, ok := byName[want.name]
			if !ok {
				t.Errorf("expected %s in resolved secrets", want.name)
				continue
			}
			if sv.Value != want.value {
				t.Errorf("%s: expected value %q, got %q", want.name, want.value, sv.Value)
			}
			if sv.SecretType != want.typ {
				t.Errorf("%s: expected type %q, got %q", want.name, want.typ, sv.SecretType)
			}
		}

		missing := SecretMeta{ID: tid("parity-missing"), Name: "GHOST", Scope: ScopeProject, ScopeID: "project-1", Version: 1}
		results, err := backend.FetchValues(ctx, []SecretMeta{ownMeta, missing})
		if err != nil {
			t.Fatalf("FetchValues failed: %v", err)
		}
		if res := results[ownMeta.ID]; res.Err != nil || res.Value != "own-value" {
			t.Errorf("own-scope FetchValues: got value=%q err=%v, want value=%q err=nil", res.Value, res.Err, "own-value")
		}
		if res := results[missing.ID]; res.Err != store.ErrNotFound {
			t.Errorf("missing-record FetchValues: got err=%v, want store.ErrNotFound", res.Err)
		}
	}

	t.Run("local", func(t *testing.T) {
		backend, s := createTestBackend(t)
		seed := func(name, scope, scopeID, value, secretType string, allowProgeny bool, createdBy string) SecretMeta {
			seedSecret(t, s, &store.Secret{
				ID:             tid("parity-local-" + name),
				Key:            name,
				EncryptedValue: value,
				SecretType:     secretType,
				Target:         name,
				Scope:          scope,
				ScopeID:        scopeID,
				AllowProgeny:   allowProgeny,
				CreatedBy:      createdBy,
			})
			meta, err := backend.GetMeta(context.Background(), name, scope, scopeID)
			if err != nil {
				t.Fatalf("GetMeta(%s) failed: %v", name, err)
			}
			return *meta
		}
		run(t, backend, seed)
	})

	t.Run("gcp", func(t *testing.T) {
		backend, _ := createTestGCPBackend(t)
		seed := func(name, scope, scopeID, value, secretType string, allowProgeny bool, createdBy string) SecretMeta {
			_, meta, err := backend.Set(context.Background(), &SetSecretInput{
				Name:         name,
				Value:        value,
				SecretType:   secretType,
				Scope:        scope,
				ScopeID:      scopeID,
				AllowProgeny: allowProgeny,
				CreatedBy:    createdBy,
			})
			if err != nil {
				t.Fatalf("Set(%s) failed: %v", name, err)
			}
			return *meta
		}
		run(t, backend, seed)
	})
}

// ============================================================================
// Get caller allow-list (drift guard)
// ============================================================================

// getCallerPattern matches a call to SecretBackend.Get through one of the
// receiver names used in this codebase for a secret.SecretBackend value:
// secretBackend (the Hub server field), sb (a local secret.SecretBackend
// variable) and Backend (an exported config field, including through a
// selector such as cfg.Backend). This is a name-based regex match, not a
// type-aware one: a new caller written under a different local name (for
// example backend.Get( or b.Get() would not be matched. _test.go files are
// not scanned, so a fake's own Get method never counts as a caller.
var getCallerPattern = regexp.MustCompile(`\b(?:secretBackend|sb|Backend)\.Get\(`)

// hubInternalGetCallers is the curated set of files outside this package
// that call SecretBackend.Get, with the number of call sites each currently
// has. Get keeps its fallback behaviour (see its doc comment) only for this
// fixed set: hub-owned infrastructure loaders, hub-side clone credentials
// that are never delivered to an agent, and agent material paths that have
// not yet switched to FetchValues. Adding a new caller, or a new call site
// in one of these files, requires a deliberate update here so the change is
// surfaced rather than silently inherited.
var hubInternalGetCallers = map[string]int{
	filepath.Join("pkg", "hub", "server.go"):                      2, // hub signing keys (current + legacy)
	filepath.Join("pkg", "hub", "oidckeys.go"):                    1, // OIDC signing key material
	filepath.Join("pkg", "hub", "handlers_chat_secrets.go"):       1, // hub-scoped chat integration secret
	filepath.Join("pkg", "hub", "handlers_github_app.go"):         1, // hub-scoped GitHub App credential
	filepath.Join("pkg", "hub", "handlers_projects_core.go"):      2, // resolveCloneToken: hub-side git clone credential, never delivered
	filepath.Join("pkg", "hub", "resource_import.go"):             1, // resource import: hub-side git clone credential, never delivered
	filepath.Join("pkg", "hub", "handlers_agent_secret_fetch.go"): 1, // pending FetchValues migration: agent runtime secret fetch
	filepath.Join("pkg", "hub", "handlers_env_secrets.go"):        1, // pending FetchValues migration: agent runtime secret get
	filepath.Join("pkg", "hub", "httpdispatcher.go"):              3, // pending FetchValues migration: provision credentials + NoAuth GITHUB_TOKEN (own and owner scope)
	filepath.Join("pkg", "secretmigration", "secretmigration.go"): 1, // hub secret migration helper
	filepath.Join("cmd", "server_foreground.go"):                  3, // hub startup: signing keys + telemetry credentials
}

// repoRootFromWD walks up from the current working directory (the package
// directory when running `go test`) to the module root, identified by
// go.mod.
func repoRootFromWD(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (go.mod not found)")
		}
		dir = parent
	}
}

// TestSecretBackendGet_CallersAreHubInternal is a drift guard for the
// contract that SecretBackend.Get is reachable only from hub-internal code:
// a fixed set of hub infrastructure loaders, hub-side clone credentials that
// are never delivered to an agent, and the material paths that have not yet
// switched to FetchValues. It scans non-test .go files under pkg/, cmd/ and
// extras/ for calls through the recognized receiver names (see below); a
// new or changed call site under those names requires a deliberate update
// to hubInternalGetCallers.
//
// The match (getCallerPattern) is a regex over the receiver name, not a
// type-aware scan of call sites: it recognizes secretBackend.Get(, sb.Get(
// and Backend.Get(, the three receiver names currently in use for a
// secret.SecretBackend value, and nothing else. _test.go files are excluded
// from the scan, so a fake or mock's Get method never counts as a caller and
// can't produce a false positive. A new caller under a different receiver
// name would pass unnoticed; secret.go's Get doc comment tells authors to
// use one of the recognized names or update this test deliberately.
func TestSecretBackendGet_CallersAreHubInternal(t *testing.T) {
	root := repoRootFromWD(t)
	found := make(map[string]int)

	for _, top := range []string{"pkg", "cmd", "extras"} {
		topDir := filepath.Join(root, top)
		if _, err := os.Stat(topDir); err != nil {
			continue
		}
		err := filepath.Walk(topDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			// This package implements SecretBackend; it is not a caller.
			if strings.HasPrefix(rel, filepath.Join("pkg", "secret")+string(filepath.Separator)) {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for _, line := range strings.Split(string(data), "\n") {
				if getCallerPattern.MatchString(line) {
					found[rel]++
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", topDir, err)
		}
	}

	var problems []string
	for file, count := range found {
		want, ok := hubInternalGetCallers[file]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: %d call site(s), not in the allow-list", file, count))
			continue
		}
		if count != want {
			problems = append(problems, fmt.Sprintf("%s: %d call site(s), expected %d", file, count, want))
		}
	}
	for file, want := range hubInternalGetCallers {
		if _, ok := found[file]; !ok {
			problems = append(problems, fmt.Sprintf("%s: expected %d call site(s), found none (update the allow-list if this caller was removed)", file, want))
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("SecretBackend.Get has callers outside the allow-list:\n%s", strings.Join(problems, "\n"))
	}
}
