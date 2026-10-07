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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#2124 / ptone/scion#2151: unit-level coverage of
// listCursorSealer, the shared AEAD helper every authorizedList /
// listAuthorizedOrAll caller now seals and opens its client-facing cursors
// through. Endpoint/handler-level coverage (opacity of a real NextCursor,
// cross-query and cross-caller rejection, page-boundary correctness) lives
// in authorized_list_cursor_opacity_test.go; this file exercises the
// primitive directly.

func mustNewListCursorSealer(t *testing.T) *listCursorSealer {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	sealer, err := newListCursorSealer(key)
	require.NoError(t, err)
	return sealer
}

func TestListCursorSealer_RoundTrip(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	inner := authorizedListCursor(time.Now(), "11111111-1111-1111-1111-111111111111", "binding-x")

	sealed, err := sealer.Seal(inner, "binding-x")
	require.NoError(t, err)
	require.NotEmpty(t, sealed)
	assert.NotEqual(t, inner, sealed, "the sealed cursor must not equal the plaintext it carries")

	opened, err := sealer.Open(sealed, "binding-x")
	require.NoError(t, err)
	assert.Equal(t, inner, opened)
}

// Every sealed cursor starts with the literal version prefix: a server can
// tell a v1 cursor from garbage, a legacy plaintext cursor, or a future
// version cheaply, without an AEAD open.
func TestListCursorSealer_SealedCursorHasVersionPrefix(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	inner := authorizedListCursor(time.Now(), "11111111-1111-1111-1111-111111111112", "binding-x")

	sealed, err := sealer.Seal(inner, "binding-x")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(sealed, listCursorPrefix), "sealed cursor %q must start with %q", sealed, listCursorPrefix)
}

// A well-formed sealed body with no version prefix at all is rejected --
// distinct from the tampered/truncated cases below, which start from a real
// prefixed cursor.
func TestListCursorSealer_MissingVersionPrefixRejected(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	inner := authorizedListCursor(time.Now(), "11111111-1111-1111-1111-111111111113", "binding-x")
	sealed, err := sealer.Seal(inner, "binding-x")
	require.NoError(t, err)

	bodyOnly := strings.TrimPrefix(sealed, listCursorPrefix)
	_, err = sealer.Open(bodyOnly, "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor)
}

// A cursor with an unrecognized (future) version prefix is rejected, not
// trial-decrypted.
func TestListCursorSealer_UnknownVersionPrefixRejected(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	inner := authorizedListCursor(time.Now(), "11111111-1111-1111-1111-111111111114", "binding-x")
	sealed, err := sealer.Seal(inner, "binding-x")
	require.NoError(t, err)

	withOtherPrefix := "c2." + strings.TrimPrefix(sealed, listCursorPrefix)
	_, err = sealer.Open(withOtherPrefix, "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor)
}

// The correct prefix with a non-base64 body is rejected the same uniform way.
func TestListCursorSealer_VersionPrefixWithNonBase64BodyRejected(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	_, err := sealer.Open(listCursorPrefix+"!!!not-base64!!!", "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor)
}

// Seal must use a fresh random nonce every call: sealing the identical
// (inner, binding) pair twice must not produce identical output, and in
// particular must not reuse the same 12-byte nonce prefix -- nonce reuse
// under a fixed key breaks AES-GCM's confidentiality and authentication
// guarantees outright.
func TestListCursorSealer_SealUsesFreshNoncePerCall(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	inner := authorizedListCursor(time.Now(), "12121212-1212-1212-1212-121212121212", "binding-x")

	sealedA, err := sealer.Seal(inner, "binding-x")
	require.NoError(t, err)
	sealedB, err := sealer.Seal(inner, "binding-x")
	require.NoError(t, err)
	assert.NotEqual(t, sealedA, sealedB, "sealing the same plaintext twice must not produce identical ciphertexts")

	rawA, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealedA, listCursorPrefix))
	require.NoError(t, err)
	rawB, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealedB, listCursorPrefix))
	require.NoError(t, err)
	nonceSize := sealer.aead.NonceSize()
	require.True(t, len(rawA) >= nonceSize && len(rawB) >= nonceSize)
	assert.NotEqual(t, rawA[:nonceSize], rawB[:nonceSize], "each Seal call must use a fresh nonce")

	// Both must still open correctly -- freshness must not have broken
	// round-tripping.
	openedA, err := sealer.Open(sealedA, "binding-x")
	require.NoError(t, err)
	assert.Equal(t, inner, openedA)
	openedB, err := sealer.Open(sealedB, "binding-x")
	require.NoError(t, err)
	assert.Equal(t, inner, openedB)
}

// TestListCursorSealer_AADBindingAloneIsAuthenticated proves the AAD really
// binds the binding string, independent of validateAuthorizedListCursor's
// defense-in-depth re-check of the binding embedded in the decrypted
// plaintext. It calls the real Seal wrapper with an inner cursor whose own
// embedded binding is "B" but sealed under AAD binding "A" -- a
// mismatch no real caller constructs (every real call site seals the same
// binding value that is already embedded in inner via authorizedListCursor),
// used here purely to isolate the AAD's contribution. It then opens the
// result asking for "B" -- the binding embedded in inner, not the one it
// was actually sealed under. If the AAD were dropped or made
// binding-independent (so Seal and Open agree with each other regardless of
// the binding argument), decryption would wrongly succeed, and the inner
// re-check would wrongly pass too, since inner really does carry "B": the
// only thing standing between that and a cross-query/cross-caller cursor
// working is the AAD actually binding "A" at seal time and "B" not matching
// it at open time.
func TestListCursorSealer_AADBindingAloneIsAuthenticated(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	const sealedUnderBinding = "templates:binding-a"
	const bindingEmbeddedInInner = "templates:binding-b"
	inner := authorizedListCursor(time.Now(), "13131313-1313-1313-1313-131313131313", bindingEmbeddedInInner)

	sealed, err := sealer.Seal(inner, sealedUnderBinding)
	require.NoError(t, err)

	_, err = sealer.Open(sealed, bindingEmbeddedInInner)
	assert.ErrorIs(t, err, errInvalidCursor,
		"a cursor sealed under binding A must not open when asked for binding B, even though the inner plaintext embeds B")
}

// assertNoRecoverableCursorPayload asserts that sealed's raw bytes (the
// version prefix stripped, then base64-decoded) contain no recoverable trace
// of the position it carries: neither the encoded inner cursor itself, nor
// that encoding's own decoded "created,id,binding" payload, nor the item ID
// or binding as bare substrings. This is stronger than tamper-evidence: an
// authenticated-but-unencrypted (signature-only) cursor format is
// tamper-evident and key-dependent, but still fails this check, because the
// payload sits in the clear. Confidentiality, not just authentication, is
// the property under test.
func assertNoRecoverableCursorPayload(t *testing.T, sealed, inner string) {
	t.Helper()
	require.True(t, strings.HasPrefix(sealed, listCursorPrefix), "sealed cursor %q must start with %q", sealed, listCursorPrefix)
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, listCursorPrefix))
	require.NoError(t, err)
	rawStr := string(raw)

	assert.NotContains(t, rawStr, inner, "raw cursor bytes must not contain the encoded inner cursor")

	decodedInner, err := base64.URLEncoding.DecodeString(inner)
	require.NoError(t, err)
	parts := strings.SplitN(string(decodedInner), ",", 3)
	require.Len(t, parts, 3, "inner must decode to created,id,binding")
	created, id, binding := parts[0], parts[1], parts[2]

	assert.NotContains(t, rawStr, string(decodedInner), "raw cursor bytes must not contain the decoded created,id,binding payload")
	assert.NotContains(t, rawStr, id, "raw cursor bytes must not contain the item ID")
	assert.NotContains(t, rawStr, created, "raw cursor bytes must not contain the created time")
	assert.NotContains(t, rawStr, binding, "raw cursor bytes must not contain the binding")
}

// The sealed cursor must never contain the item's ID, created time, or
// binding as a literal, readable substring: Seal's whole purpose is
// confidentiality, not just tamper-evidence (a plain signature would
// satisfy tamper-evidence alone).
func TestListCursorSealer_SealedFormDoesNotContainPlaintext(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	const id = "22222222-2222-2222-2222-222222222222"
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	binding := "templates:some-authorized-scope-hash"
	inner := authorizedListCursor(created, id, binding)

	sealed, err := sealer.Seal(inner, binding)
	require.NoError(t, err)

	assert.NotContains(t, sealed, id)
	assert.NotContains(t, sealed, created.Format(time.RFC3339Nano))
	assert.NotContains(t, sealed, binding)
	assert.NotContains(t, sealed, inner)
	// Nor any byte-reordering artifact: check the raw decoded bytes too, not
	// just the base64 text (after stripping the version prefix, which is not
	// itself base64).
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, listCursorPrefix))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), id)
	assert.NotContains(t, string(raw), binding)

	// The stronger, general-purpose check: the payload itself (not just its
	// individually-known fields) is unrecoverable from the raw bytes. A
	// signature-only (authenticated but unencrypted) cursor format passes
	// every assertion above except this one.
	assertNoRecoverableCursorPayload(t, sealed, inner)
}

func TestListCursorSealer_WrongBindingRejected(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	inner := authorizedListCursor(time.Now(), "33333333-3333-3333-3333-333333333333", "binding-a")
	sealed, err := sealer.Seal(inner, "binding-a")
	require.NoError(t, err)

	_, err = sealer.Open(sealed, "binding-b")
	assert.ErrorIs(t, err, errInvalidCursor)
}

// TestListCursorSealer_SessionAndScopedUATBindingsForSameUserDiffer isolates
// scopedCursorBinding's identity component on a filter held constant (unlike
// TestListTemplatesCursor_CrossCallerReuseRejected, whose scope=user filter
// also varies with the caller): a session (AuthenticatedUser) and a scoped
// UAT (ScopedUserIdentity) for the identical user ID must not share a
// cursor, since a scoped credential's authority ceiling differs from the
// full session's even though the underlying user is the same.
func TestListCursorSealer_SessionAndScopedUATBindingsForSameUserDiffer(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	filter := store.TemplateFilter{Scope: "project", ScopeID: "project-1", Status: store.TemplateStatusActive}
	session := NewAuthenticatedUser("shared-user-1", "shared-user-1@test.com", "Shared User", store.UserRoleMember, "api")
	scopedUAT := NewScopedUserIdentityWithCredentialID(session, "project-1", []string{"template:read"}, "uat-1")

	sessionBinding := scopedCursorBinding("templates", filter, session)
	scopedBinding := scopedCursorBinding("templates", filter, scopedUAT)
	require.NotEqual(t, sessionBinding, scopedBinding,
		"a session identity and a scoped UAT for the same user must not compute the same binding")

	inner := authorizedListCursor(time.Now(), "11111111-1111-1111-1111-111111111115", sessionBinding)
	sealed, err := sealer.Seal(inner, sessionBinding)
	require.NoError(t, err)

	_, err = sealer.Open(sealed, scopedBinding)
	assert.ErrorIs(t, err, errInvalidCursor, "a session cursor must be rejected when replayed under a scoped-UAT binding for the same user")

	opened, err := sealer.Open(sealed, sessionBinding)
	require.NoError(t, err)
	assert.Equal(t, inner, opened, "sanity: the cursor still opens under the binding it was actually sealed under")
}

func TestListCursorSealer_TamperedByteRejected(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	inner := authorizedListCursor(time.Now(), "44444444-4444-4444-4444-444444444444", "binding-x")
	sealed, err := sealer.Seal(inner, "binding-x")
	require.NoError(t, err)

	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, listCursorPrefix))
	require.NoError(t, err)
	require.NotEmpty(t, raw)
	tampered := append([]byte{}, raw...)
	tampered[len(tampered)-1] ^= 0x01
	tamperedSealed := listCursorPrefix + base64.RawURLEncoding.EncodeToString(tampered)

	_, err = sealer.Open(tamperedSealed, "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor)
}

func TestListCursorSealer_TruncatedRejected(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	inner := authorizedListCursor(time.Now(), "55555555-5555-5555-5555-555555555555", "binding-x")
	sealed, err := sealer.Seal(inner, "binding-x")
	require.NoError(t, err)

	for _, cut := range []int{0, 1, len(sealed) / 2, len(sealed) - 1} {
		truncated := sealed[:cut]
		_, err := sealer.Open(truncated, "binding-x")
		assert.ErrorIs(t, err, errInvalidCursor, "cut to %d chars", cut)
	}
}

func TestListCursorSealer_NonBase64Rejected(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	_, err := sealer.Open("!!!not-base64!!!", "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor)
}

func TestListCursorSealer_EmptyStringRejected(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	_, err := sealer.Open("", "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor)
}

// A different key (as after key rotation, or a different hub replica) can
// never open what another key sealed.
func TestListCursorSealer_DifferentKeyRejected(t *testing.T) {
	a := mustNewListCursorSealer(t)
	b := mustNewListCursorSealer(t)
	inner := authorizedListCursor(time.Now(), "66666666-6666-6666-6666-666666666666", "binding-x")
	sealed, err := a.Seal(inner, "binding-x")
	require.NoError(t, err)

	_, err = b.Open(sealed, "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor)
}

// A version/domain change (the AAD prefix listCursorSealDomain) invalidates
// every previously issued cursor the same way a key rotation does -- this
// simulates that by sealing under a different domain-separation prefix than
// Open uses.
func TestListCursorSealer_DomainVersionMismatchRejected(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	inner := authorizedListCursor(time.Now(), "77777777-7777-7777-7777-777777777777", "binding-x")

	nonce := make([]byte, sealer.aead.NonceSize())
	_, err := rand.Read(nonce)
	require.NoError(t, err)
	ciphertext := sealer.aead.Seal(nil, nonce, []byte(inner), []byte("scion-list-cursor-v2:binding-x"))
	sealedUnderOtherVersion := listCursorPrefix + base64.RawURLEncoding.EncodeToString(append(nonce, ciphertext...))

	_, err = sealer.Open(sealedUnderOtherVersion, "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor)
}

// Legacy (pre-opaque-cursor) plaintext cursors must fail the same uniform
// way, not crash or silently pass through as if they were sealed.
func TestListCursorSealer_LegacyPlaintextCursorRejected(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	legacy := authorizedListCursor(time.Now(), "88888888-8888-8888-8888-888888888888", "binding-x")

	_, err := sealer.Open(legacy, "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor)
}

// openAndValidateListCursor's empty-cursor short circuit means a fresh
// first-page request always succeeds regardless of sealer state -- even a
// nil sealer (e.g. a server that failed to provision one) never blocks a
// first page, only a resume.
func TestOpenAndValidateListCursor_EmptyCursorAlwaysValid(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	got, err := openAndValidateListCursor(sealer, "", "any-binding")
	require.NoError(t, err)
	assert.Equal(t, "", got)

	got, err = openAndValidateListCursor(nil, "", "any-binding")
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

func TestOpenAndValidateListCursor_NilSealerRejectsNonEmptyCursor(t *testing.T) {
	_, err := openAndValidateListCursor(nil, "some-cursor", "any-binding")
	assert.ErrorIs(t, err, errInvalidCursor)
}

// Seal and Open must behave the same way on a nil sealer -- fail closed
// (an error the caller turns into a response with no cursor emitted),
// never a nil-pointer panic. A bare Server{} (or, defensively, a direct
// call that skips past openAndValidateListCursor's own nil check) must not
// crash either helper.
func TestListCursorSealer_NilReceiverFailsClosedWithoutPanicking(t *testing.T) {
	var sealer *listCursorSealer

	assert.NotPanics(t, func() {
		_, err := sealer.Seal("inner", "binding-x")
		assert.ErrorIs(t, err, errListCursorSealerUnavailable)
	})
	assert.NotPanics(t, func() {
		_, err := sealer.Open("c1.anything", "binding-x")
		assert.ErrorIs(t, err, errInvalidCursor)
	})
}

// A nil sealer reaching listAuthorizedOrAll's direct-query path (the state
// a bare Server{} would be in) fails the request closed instead of
// nil-dereferencing when there is a next page to seal.
func TestListAuthorizedOrAll_NilSealerFailsClosedInsteadOfPanicking(t *testing.T) {
	type nilSealerTestItem struct{ id string }
	list := func(_ context.Context, _ store.ListOptions) (*store.ListResult[nilSealerTestItem], error) {
		return &store.ListResult[nilSealerTestItem]{
			Items:      []nilSealerTestItem{{id: "only-item"}},
			NextCursor: "more",
			TotalCount: 1,
		}, nil
	}
	resource := func(it *nilSealerTestItem) Resource { return Resource{Type: "test", ID: it.id} }
	cursorFor := func(it *nilSealerTestItem) string { return it.id }

	var panicked bool
	var result authorizedListResult[nilSealerTestItem]
	var err error
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		result, err = listAuthorizedOrAll[nilSealerTestItem](
			context.Background(), nil, "", 1, "binding", nil, false,
			list, resource, cursorFor, nil,
		)
	}()
	assert.False(t, panicked, "a nil sealer with a next page must fail closed, not panic")
	require.Error(t, err)
	assert.Empty(t, result.NextCursor, "no cursor may be emitted when it could not be sealed")
}

// Seal/Open round-trip through openAndValidateListCursor exactly as a
// handler uses it: seal an item cursor, then recover it on the next page.
func TestOpenAndValidateListCursor_RoundTrip(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	binding := "templates:filter-hash"
	inner := authorizedListCursor(time.Now(), "99999999-9999-9999-9999-999999999999", binding)
	sealed, err := sealer.Seal(inner, binding)
	require.NoError(t, err)

	opened, err := openAndValidateListCursor(sealer, sealed, binding)
	require.NoError(t, err)
	assert.Equal(t, inner, opened)
}

// ---------------------------------------------------------------------------
// Key provisioning: persistence, rotation and length enforcement
// (ptone/scion#2151).
// ---------------------------------------------------------------------------

func TestListCursorKey_ProvisionedAndPersisted(t *testing.T) {
	srv, _ := testServer(t)
	require.NotNil(t, srv.listCursorSealer)

	// It is dedicated: the raw key bytes actually differ from the
	// download-signing key's, not just "some other 32 zero-ish bytes that
	// can never match" -- and a cursor sealed by this server's sealer must
	// not open under a sealer built from the download-signing key either.
	listKey, err := srv.ensureSigningKey(context.Background(), SecretKeyListCursorKey, nil)
	require.NoError(t, err)
	require.NotEmpty(t, srv.downloadSigningKey)
	assert.False(t, bytes.Equal(listKey, srv.downloadSigningKey),
		"list_cursor_key must not equal the download-signing key")

	other, err := newListCursorSealer(srv.downloadSigningKey)
	require.NoError(t, err)
	inner := authorizedListCursor(time.Now(), "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "b")
	sealed, err := srv.listCursorSealer.Seal(inner, "b")
	require.NoError(t, err)
	_, err = other.Open(sealed, "b")
	assert.ErrorIs(t, err, errInvalidCursor)

	// It is persisted: resolving the key again yields the same bytes, so
	// outstanding cursors survive a Hub restart.
	key1, err := srv.ensureSigningKey(context.Background(), SecretKeyListCursorKey, nil)
	require.NoError(t, err)
	key2, err := srv.ensureSigningKey(context.Background(), SecretKeyListCursorKey, nil)
	require.NoError(t, err)
	assert.Equal(t, key1, key2)
	require.Len(t, key1, 32)
}

// Under a deployment-wide SharedSigningSecret, the list-cursor key is
// derived deterministically (so two independent server instances -- even
// with different HubIDs, standing in for separate replicas -- agree on it
// and can open each other's cursors) and key-name-separated from the
// download-signing key derived from the same secret.
func TestListCursorKey_SharedSigningSecretDerivesConsistentKeyNameSeparatedFromDownload(t *testing.T) {
	const sharedSecret = "list-cursor-shared-signing-secret-test"

	newSrv := func(hubID string) *Server {
		s, err := newTestStore(t, ":memory:")
		if err != nil {
			if strings.Contains(err.Error(), "sqlite driver not registered") {
				t.Skip("Skipping test because sqlite driver is not registered (build with -tags sqlite to enable)")
			}
			t.Fatalf("failed to create test store: %v", err)
		}
		require.NoError(t, s.Migrate(context.Background()))

		cfg := DefaultServerConfig()
		cfg.HubID = hubID
		cfg.SharedSigningSecret = sharedSecret
		srv, err := New(cfg, s)
		require.NoError(t, err)
		t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
		return srv
	}

	srv1 := newSrv("list-cursor-shared-secret-hub-1")
	srv2 := newSrv("list-cursor-shared-secret-hub-2")
	require.NotNil(t, srv1.listCursorSealer)
	require.NotNil(t, srv2.listCursorSealer)

	binding := "templates:shared-secret-test"
	inner := authorizedListCursor(time.Now(), "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee", binding)
	sealed, err := srv1.listCursorSealer.Seal(inner, binding)
	require.NoError(t, err)

	// A cursor sealed by one HubID's server opens on the other -- the
	// derived key does not depend on the (differing) HubID.
	opened, err := srv2.listCursorSealer.Open(sealed, binding)
	require.NoError(t, err, "a cursor sealed under a shared-secret-derived key must open on any replica sharing that secret")
	assert.Equal(t, inner, opened)

	// The derived list-cursor key itself differs from the derived
	// download-signing key under the same shared secret: key-name
	// separation holds even in the deterministic-derivation path, not just
	// the per-host generated-key path.
	listKey, err := srv1.ensureSigningKey(context.Background(), SecretKeyListCursorKey, nil)
	require.NoError(t, err)
	assert.Equal(t, deriveSharedSigningKey(sharedSecret, SecretKeyListCursorKey), listKey)
	assert.NotEqual(t,
		deriveSharedSigningKey(sharedSecret, SecretKeyDownloadSigningKey),
		listKey,
		"list_cursor_key and download_signing_key must derive to different bytes under the same shared secret")
}

// The same persisted key opens cursors across independent sealer instances
// (standing in for separate hub replicas or a restart), and a genuinely
// different key does not -- while a fresh first-page request is unaffected
// either way.
func TestListCursorKey_PersistsAcrossSealerInstances(t *testing.T) {
	srv, _ := testServer(t)

	key, err := srv.ensureSigningKey(context.Background(), SecretKeyListCursorKey, nil)
	require.NoError(t, err)
	independentSealer, err := newListCursorSealer(key)
	require.NoError(t, err)

	binding := "groups:filter-hash"
	inner := authorizedListCursor(time.Now(), "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", binding)
	sealed, err := srv.listCursorSealer.Seal(inner, binding)
	require.NoError(t, err)

	opened, err := independentSealer.Open(sealed, binding)
	require.NoError(t, err, "the persisted key must open a cursor sealed by the original server instance")
	assert.Equal(t, inner, opened)

	// A changed key (rotation, or a replica that hasn't converged) returns
	// the uniform invalid-cursor error...
	changedKey := bytes.Repeat([]byte{0x11}, 32)
	changedSealer, err := newListCursorSealer(changedKey)
	require.NoError(t, err)
	_, err = changedSealer.Open(sealed, binding)
	assert.ErrorIs(t, err, errInvalidCursor)

	// ...while a fresh first-page request (empty cursor) is still usable
	// against the changed sealer.
	firstPage, err := openAndValidateListCursor(changedSealer, "", binding)
	require.NoError(t, err)
	assert.Equal(t, "", firstPage)
}

// initListCursorSealer follows the download-signing key's persistence path
// end to end: the same store, same hub ID, and a second server picks up the
// identical key on "restart" (a second New() against the same store).
func TestListCursorKey_PersistsAcrossServerRestart(t *testing.T) {
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping test because sqlite driver is not registered (build with -tags sqlite to enable)")
		}
		t.Fatalf("failed to create test store: %v", err)
	}
	require.NoError(t, s.Migrate(context.Background()))

	const hubID = "list-cursor-persist-hub"
	newSrv := func() *Server {
		cfg := DefaultServerConfig()
		cfg.HubID = hubID
		srv, err := New(cfg, s)
		require.NoError(t, err)
		t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
		return srv
	}

	srv1 := newSrv()
	srv2 := newSrv()
	require.NotNil(t, srv1.listCursorSealer)
	require.NotNil(t, srv2.listCursorSealer)

	binding := "harness-configs:filter-hash"
	inner := authorizedListCursor(time.Now(), "cccccccc-cccc-cccc-cccc-cccccccccccc", binding)
	sealed, err := srv1.listCursorSealer.Seal(inner, binding)
	require.NoError(t, err)

	opened, err := srv2.listCursorSealer.Open(sealed, binding)
	require.NoError(t, err, "a cursor sealed before a restart must still open after it")
	assert.Equal(t, inner, opened)
}

// When the deployment requires stable keys, a missing list-cursor key fails
// startup (like the agent/user/download-signing keys) instead of silently
// using a per-replica ephemeral key that would cause intermittent
// invalid-cursor 400s.
func TestListCursorKey_FailFastWhenStableKeysRequired(t *testing.T) {
	srv, _ := testServer(t)
	for _, scopeID := range []string{srv.hubID, "hub", ""} {
		_ = srv.store.DeleteSecret(context.Background(), SecretKeyListCursorKey, store.ScopeHub, scopeID)
	}
	srv.listCursorSealer = nil
	srv.config.RequireStableSigningKey = true
	err := srv.initListCursorSealer(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list cursor key")
	assert.Nil(t, srv.listCursorSealer)

	// Without the stable-key requirement, the same situation provisions a
	// fresh (ephemeral) key instead of failing.
	srv.config.RequireStableSigningKey = false
	require.NoError(t, srv.initListCursorSealer(context.Background()))
	assert.NotNil(t, srv.listCursorSealer)
}

// A wrong-length key (neither missing nor a decode error, just not
// 32 bytes -- e.g. a pre-existing secret of the wrong size, or a
// misconfigured SharedSigningSecret-derived value from a future change)
// follows the exact same stable-key failure policy as a missing key.
func TestListCursorKey_WrongLengthFollowsStableKeyPolicy(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()
	for _, scopeID := range []string{srv.hubID, "hub", ""} {
		_ = srv.store.DeleteSecret(ctx, SecretKeyListCursorKey, store.ScopeHub, scopeID)
	}
	// Seed a wrong-length (16-byte) key directly where ensureSigningKey will
	// find it, instead of the normal 32-byte generation path.
	shortKey := bytes.Repeat([]byte{0x09}, 16)
	require.NoError(t, srv.backupSigningKeyToStore(ctx, SecretKeyListCursorKey, base64.StdEncoding.EncodeToString(shortKey), srv.hubID))

	srv.listCursorSealer = nil
	srv.config.RequireStableSigningKey = true
	err := srv.initListCursorSealer(ctx)
	require.Error(t, err, "a wrong-length persisted key must fail startup under RequireStableSigningKey")
	assert.Contains(t, err.Error(), "list cursor key")
	assert.Nil(t, srv.listCursorSealer)

	// Without the stable-key requirement, the same wrong-length key falls
	// back to a fresh ephemeral 32-byte key with a warning, rather than
	// constructing an AEAD with the wrong key size.
	srv.config.RequireStableSigningKey = false
	require.NoError(t, srv.initListCursorSealer(ctx))
	require.NotNil(t, srv.listCursorSealer)
}

// Neither Seal, Open, nor key initialization ever logs key material or
// decrypted cursor contents (ptone/scion#2151).
func TestListCursorSealer_NeverLogsKeyOrPlaintext(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	srv, _ := testServer(t)
	binding := "templates:filter-hash"
	inner := authorizedListCursor(time.Now(), "dddddddd-dddd-dddd-dddd-dddddddddddd", binding)
	sealed, err := srv.listCursorSealer.Seal(inner, binding)
	require.NoError(t, err)
	opened, err := srv.listCursorSealer.Open(sealed, binding)
	require.NoError(t, err)
	require.Equal(t, inner, opened)

	// Force a fresh init cycle (covers the log lines ensureSigningKey /
	// initListCursorSealer emit) too.
	require.NoError(t, srv.initListCursorSealer(context.Background()))

	out := buf.String()
	assert.NotContains(t, out, inner, "log output must not contain the decrypted cursor plaintext")
	assert.NotContains(t, out, sealed, "log output must not contain the raw sealed cursor either")
}

// TestListCursorSealer_NeverLogsKeyMaterialOrCursorContentsAcrossHTTPLifecycle
// is the non-vacuous form of the check above: it captures logs (at Debug, so
// nothing is filtered by level) across a real server New() (init), a sealed
// first page, a resumed page and a rejected cursor, all through the HTTP
// handler, and checks the raw key bytes in every encoding a log line might
// render them in (std base64, URL base64, hex), not just the two ad hoc
// string forms the unit-level test happens to construct. The position and
// item ID it checks for are derived from the cursor the walk actually
// produced -- opened with the endpoint's own binding -- rather than from a
// seeded value the walk may or may not reach.
func TestListCursorSealer_NeverLogsKeyMaterialOrCursorContentsAcrossHTTPLifecycle(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	// setupTemplateScopeTest (template_scope_authz_test.go) calls New()
	// internally, so the init-time ensureSigningKey/initListCursorSealer log
	// lines land in buf too.
	srv, s, _, carol, _ := setupTemplateScopeTest(t)
	created := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	require.NoError(t, s.CreateTemplate(context.Background(), &store.Template{
		ID:          api.NewUUID(),
		Name:        "log-lifecycle-a",
		Slug:        api.Slugify("log-lifecycle-a"),
		Scope:       store.TemplateScopeUser,
		ScopeID:     carol.ID,
		OwnerID:     carol.ID,
		Status:      "active",
		StoragePath: "templates/user/log-lifecycle-a",
		Created:     created,
		Updated:     created,
	}))
	require.NoError(t, s.CreateTemplate(context.Background(), &store.Template{
		ID:          api.NewUUID(),
		Name:        "log-lifecycle-b",
		Slug:        api.Slugify("log-lifecycle-b"),
		Scope:       store.TemplateScopeUser,
		ScopeID:     carol.ID,
		OwnerID:     carol.ID,
		Status:      "active",
		StoragePath: "templates/user/log-lifecycle-b",
		Created:     created.Add(-time.Second),
		Updated:     created.Add(-time.Second),
	}))

	key, err := srv.ensureSigningKey(context.Background(), SecretKeyListCursorKey, nil)
	require.NoError(t, err)
	require.Len(t, key, 32)

	rec1, first := getTemplatesPage(t, srv, carol, "scope=user&limit=1")
	require.Equal(t, http.StatusOK, rec1.Code, rec1.Body.String())
	require.NotEmpty(t, first.NextCursor)

	// Derive the position and item ID the cursor actually carries -- not a
	// seeded value that may not be the one this walk reached -- by opening
	// it with the real binding the endpoint computed.
	binding := scopedCursorBindingForTemplatesTest(carol, "user", carol.ID)
	inner, err := srv.listCursorSealer.Open(first.NextCursor, binding)
	require.NoError(t, err)
	rawInner, err := base64.URLEncoding.DecodeString(inner)
	require.NoError(t, err)
	innerParts := strings.SplitN(string(rawInner), ",", 3)
	require.Len(t, innerParts, 3)
	cursorCreated, cursorID := innerParts[0], innerParts[1]

	rec2, _ := getTemplatesPage(t, srv, carol, "scope=user&limit=1&cursor="+first.NextCursor)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	rec3, _ := getTemplatesPage(t, srv, carol, "scope=user&limit=1&cursor=not-a-real-cursor")
	require.Equal(t, http.StatusBadRequest, rec3.Code)

	out := buf.String()
	for _, encoded := range []string{
		base64.StdEncoding.EncodeToString(key),
		base64.URLEncoding.EncodeToString(key),
		base64.RawStdEncoding.EncodeToString(key),
		base64.RawURLEncoding.EncodeToString(key),
		hex.EncodeToString(key),
	} {
		assert.NotContains(t, out, encoded, "log output must not contain the raw key in any encoding")
	}
	assert.NotContains(t, out, inner, "log output must not contain the decrypted inner cursor")
	assert.NotContains(t, out, cursorID, "log output must not contain the item ID the cursor actually carries, at any log level")
	assert.NotContains(t, out, cursorCreated, "log output must not contain the created time the cursor actually carries, at any log level")
	assert.NotContains(t, out, first.NextCursor, "log output must not contain the sealed cursor")
}
