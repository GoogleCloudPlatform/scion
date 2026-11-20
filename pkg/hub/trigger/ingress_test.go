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

package trigger_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestSignAndVerifyScionV1(t *testing.T) {
	// Representative 32-byte random secret encoded as RawURLEncoding (what 1b-1 generates).
	rawKeyBytes := []byte("0123456789abcdef0123456789abcdef")
	secret := base64.RawURLEncoding.EncodeToString(rawKeyBytes)
	now := time.Unix(1760000000, 0).UTC()
	body := []byte(`{"status":"ok","commit":"abc123"}`)

	t.Run("minimal headers round-trip", func(t *testing.T) {
		sig := trigger.SignScionV1(secret, now.Unix(), "", "", "", body)
		assert.True(t, trigger.VerifyScionV1Signature(secret, now, sig, "", "", "", body))
	})

	t.Run("all routing headers round-trip", func(t *testing.T) {
		sig := trigger.SignScionV1(secret, now.Unix(), "deliv-42", "deploy.finished", "prod/api", body)
		assert.True(t, trigger.VerifyScionV1Signature(secret, now, sig, "deliv-42", "deploy.finished", "prod/api", body))
	})

	t.Run("CLI sender example canonical string formula matches VerifyScionV1Signature", func(t *testing.T) {
		// Exact canonical string from §8.1a and cmd/trigger_webhook.go:
		// printf 'v1\n%s\n%s\n%s\n%s\n%s' "$TS" "$DELIVERY" "$EVENT" "$SUBJECT" "$BODY"
		//   | openssl dgst -sha256 -hmac "$SECRET"
		ts := strconv.FormatInt(now.Unix(), 10)
		delivery := "delivery-cli-1"
		event := "ci.completed"
		subject := "repo/main"
		canonical := fmt.Sprintf("v1\n%s\n%s\n%s\n%s\n%s", ts, delivery, event, subject, string(body))
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(canonical))
		header := "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
		assert.True(t, trigger.VerifyScionV1Signature(secret, now, header, delivery, event, subject, body))
	})

	t.Run("rev 8.15 key is ASCII secret text, not base64-decoded bytes", func(t *testing.T) {
		ts := strconv.FormatInt(now.Unix(), 10)
		canonical := fmt.Sprintf("v1\n%s\n\n\n\n%s", ts, string(body))
		decodedMAC := hmac.New(sha256.New, rawKeyBytes)
		_, _ = decodedMAC.Write([]byte(canonical))
		wrongKeyModeHeader := "t=" + ts + ",v1=" + hex.EncodeToString(decodedMAC.Sum(nil))
		assert.False(t, trigger.VerifyScionV1Signature(secret, now, wrongKeyModeHeader, "", "", "", body),
			"HMAC computed with base64-decoded secret bytes must be rejected")
	})

	t.Run("timestamp replay window boundaries (±300s)", func(t *testing.T) {
		sigMinus300 := trigger.SignScionV1(secret, now.Unix()-300, "", "", "", body)
		sigPlus300 := trigger.SignScionV1(secret, now.Unix()+300, "", "", "", body)
		sigMinus301 := trigger.SignScionV1(secret, now.Unix()-301, "", "", "", body)
		sigPlus301 := trigger.SignScionV1(secret, now.Unix()+301, "", "", "", body)

		assert.True(t, trigger.VerifyScionV1Signature(secret, now, sigMinus300, "", "", "", body))
		assert.True(t, trigger.VerifyScionV1Signature(secret, now, sigPlus300, "", "", "", body))
		assert.False(t, trigger.VerifyScionV1Signature(secret, now, sigMinus301, "", "", "", body))
		assert.False(t, trigger.VerifyScionV1Signature(secret, now, sigPlus301, "", "", "", body))
	})

	t.Run("re-labelled headers or altered body rejected", func(t *testing.T) {
		sig := trigger.SignScionV1(secret, now.Unix(), "d-1", "deploy.ok", "svc-a", body)
		assert.False(t, trigger.VerifyScionV1Signature(secret, now, sig, "d-2", "deploy.ok", "svc-a", body))
		assert.False(t, trigger.VerifyScionV1Signature(secret, now, sig, "", "deploy.ok", "svc-a", body))
		assert.False(t, trigger.VerifyScionV1Signature(secret, now, sig, "d-1", "deploy.failed", "svc-a", body))
		assert.False(t, trigger.VerifyScionV1Signature(secret, now, sig, "d-1", "deploy.ok", "svc-b", body))
		assert.False(t, trigger.VerifyScionV1Signature(secret, now, sig, "d-1", "deploy.ok", "svc-a", []byte(`{"status":"tampered"}`)))
		// Moving bytes across field boundaries cannot collide because LF separates fields.
		sigBoundary := trigger.SignScionV1(secret, now.Unix(), "ab", "cd", "", body)
		assert.False(t, trigger.VerifyScionV1Signature(secret, now, sigBoundary, "a", "bcd", "", body))
	})

	t.Run("multiple v1 tags during rotation and malformed signature headers", func(t *testing.T) {
		oldSig := trigger.SignScionV1("retired-secret", now.Unix(), "d", "ev", "sub", body)
		curSig := trigger.SignScionV1(secret, now.Unix(), "d", "ev", "sub", body)
		_, oldV1, _ := strings.Cut(oldSig, ",v1=")
		_, curV1, _ := strings.Cut(curSig, ",v1=")
		combined := fmt.Sprintf("t=%d,v1=%s,v1=%s,v2=deadbeef", now.Unix(), oldV1, curV1)
		assert.True(t, trigger.VerifyScionV1Signature(secret, now, combined, "d", "ev", "sub", body))

		bothWrong := fmt.Sprintf("t=%d,v1=%s,v1=%s", now.Unix(), oldV1, strings.Repeat("0", 64))
		assert.False(t, trigger.VerifyScionV1Signature(secret, now, bothWrong, "d", "ev", "sub", body))

		// Duplicate t= is rejected even if one matches.
		dupTS := fmt.Sprintf("t=%d,t=%d,v1=%s", now.Unix(), now.Unix(), curV1)
		assert.False(t, trigger.VerifyScionV1Signature(secret, now, dupTS, "d", "ev", "sub", body))

		// Leading zero or + sign in t= is rejected (canonical FormatInt match required).
		leadingZeroTS := fmt.Sprintf("t=0%d,v1=%s", now.Unix(), curV1)
		assert.False(t, trigger.VerifyScionV1Signature(secret, now, leadingZeroTS, "d", "ev", "sub", body))

		assert.False(t, trigger.VerifyScionV1Signature(secret, now, "", "d", "ev", "sub", body))
		assert.False(t, trigger.VerifyScionV1Signature("", now, curSig, "d", "ev", "sub", body))
		assert.False(t, trigger.VerifyScionV1Signature(secret, now, "garbage", "d", "ev", "sub", body))

		// Over-large signed header total (> 16 KiB) rejected before hashing.
		hugeSubject := strings.Repeat("a", 17*1024)
		assert.False(t, trigger.VerifyScionV1Signature(secret, now, curSig, "", "", hugeSubject, body))
	})
}

func TestIsHooksRequestPath(t *testing.T) {
	validID := "550e8400-e29b-41d4-a716-446655440000"
	assert.True(t, trigger.IsHooksRequestPath("/api/v1/hooks/"+validID))

	invalidPaths := []string{
		"",
		"/api/v1/hooks",
		"/api/v1/hooks/",
		"/api/v1/hooks/x/y",
		"/api/v1/hooks/" + validID + "/",
		"/api/v1/hooks/" + validID + "/extra",
		"/api/v1/hooks/" + strings.ToUpper(validID),
		"/api/v1/hooks/550e8400e29b41d4a7164466554400000000", // 36 hex chars, no hyphens -> still matches char class, checked in handler
		"/api/v1/hooks/not-a-uuid",
		"/api/v1/projects/p1/webhook-endpoints",
	}
	for _, p := range invalidPaths {
		if p == "/api/v1/hooks/550e8400e29b41d4a7164466554400000000" {
			// Matches regex ^/api/v1/hooks/[0-9a-f-]{36}$ for unauth dispatch, then handler rejects non-UUID with 404.
			assert.True(t, trigger.IsHooksRequestPath(p))
			continue
		}
		assert.False(t, trigger.IsHooksRequestPath(p), "path %q must not match exact hooks pattern", p)
	}
}

func TestClientRateKey_IPv4AndIPv6Slash64(t *testing.T) {
	assert.Equal(t, "203.0.113.9", trigger.ClientRateKey("203.0.113.9"))
	assert.Equal(t, "203.0.113.9", trigger.ClientRateKey("::ffff:203.0.113.9"))

	k1 := trigger.ClientRateKey("2001:db8:abcd:0012::1")
	k2 := trigger.ClientRateKey("2001:db8:abcd:0012:ffff:ffff:ffff:ffff")
	k3 := trigger.ClientRateKey("2001:db8:abcd:0013::1")
	assert.Equal(t, "2001:db8:abcd:12::/64", k1)
	assert.Equal(t, k1, k2, "distinct /128 hosts in same /64 must share one per-IP bucket key")
	assert.NotEqual(t, k1, k3, "different /64 prefixes must have distinct bucket keys")
}

func TestIngressLimiter_SplitBucketsRefillAndCleanup(t *testing.T) {
	now := time.Date(2026, 11, 16, 15, 0, 0, 0, time.UTC)
	lim := trigger.NewIngressLimiter(trigger.LimiterConfig{
		PerIPRate:               2,
		PerIPBurst:              2,
		RejectionRate:           2,
		RejectionBurst:          2,
		PerEndpointRate:         2,
		PerEndpointBurst:        2,
		PerProjectVerifiedRate:  2,
		PerProjectVerifiedBurst: 2,
		HubVerifiedRate:         3,
		HubVerifiedBurst:        3,
		MaxAge:                  10 * time.Minute,
		Now:                     func() time.Time { return now },
	})

	// Two distinct IPv6 /128 addresses in the same /64 share the burst of 2.
	assert.True(t, lim.AllowClientIP("2001:db8:1:2::1"))
	assert.True(t, lim.AllowClientIP("2001:db8:1:2::9999"))
	assert.False(t, lim.AllowClientIP("2001:db8:1:2::abcd"), "third request from same /64 must be rate-limited")
	// Another /64 is unaffected.
	assert.True(t, lim.AllowClientIP("2001:db8:1:3::1"))

	// Exhausting hub-wide rejection bucket leaves endpoint, project-verified, and hub-verified untouched.
	assert.True(t, lim.AllowRejection())
	assert.True(t, lim.AllowRejection())
	assert.False(t, lim.AllowRejection())

	assert.True(t, lim.AllowEndpoint("ep-a"))
	assert.True(t, lim.AllowEndpoint("ep-a"))
	assert.False(t, lim.AllowEndpoint("ep-a"))
	assert.True(t, lim.AllowEndpoint("ep-b"), "exhausting ep-a must not affect ep-b")

	assert.True(t, lim.AllowProjectVerified("proj-a"))
	assert.True(t, lim.AllowProjectVerified("proj-a"))
	assert.False(t, lim.AllowProjectVerified("proj-a"))
	assert.True(t, lim.AllowProjectVerified("proj-b"), "exhausting proj-a must not affect proj-b")

	assert.True(t, lim.AllowHubVerified())
	assert.True(t, lim.AllowHubVerified())
	assert.True(t, lim.AllowHubVerified())
	assert.False(t, lim.AllowHubVerified())

	// Advance clock by 500ms (2 req/s -> +1 token) and verify refill.
	now = now.Add(500 * time.Millisecond)
	assert.True(t, lim.AllowEndpoint("ep-a"))
	assert.False(t, lim.AllowEndpoint("ep-a"))

	// Advance past MaxAge; on-demand refill restores full burst without any background Cleanup sweep.
	now = now.Add(15 * time.Minute)
	assert.True(t, lim.AllowClientIP("2001:db8:1:2::1"))
	assert.True(t, lim.AllowClientIP("2001:db8:1:2::2"))
	assert.False(t, lim.AllowClientIP("2001:db8:1:2::3"))
}

func TestIngressLimiter_IPv6Slash48SprayDoesNotLockOutNewSenders(t *testing.T) {
	t.Run("sustained /48 spray (~5.5 req/s) evicts refilled buckets and admits new senders", func(t *testing.T) {
		now := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)
		lim := trigger.NewIngressLimiter(trigger.LimiterConfig{
			Now: func() time.Time { return now },
		})

		// Spray 15,000 distinct /64 subnets from one IPv6 /48 at ~5.55 req/s
		// (180ms apart -> 10,000 subnets in 1,800s = 30m). Previously, every
		// new sender IP after #10,000 was refused for up to 30m.
		for i := 0; i < trigger.LimiterMaxEntries+5000; i++ {
			subnetIP := fmt.Sprintf("2001:db8:cafe:%04x::1", i)
			require.True(t, lim.AllowClientIP(subnetIP), "spray request %d", i)
			if i == trigger.LimiterMaxEntries {
				// Legitimate new IPv4 and IPv6 senders right past the 10,000 mark must be admitted.
				assert.True(t, lim.AllowClientIP("198.51.100.55"))
				assert.True(t, lim.AllowClientIP("2001:db8:9999:1::1"))
				assert.LessOrEqual(t, lim.PerIPLen(), trigger.LimiterMaxEntries)
			}
			now = now.Add(180 * time.Millisecond)
		}

		assert.True(t, lim.AllowClientIP("203.0.113.77"))
		assert.LessOrEqual(t, lim.PerIPLen(), trigger.LimiterMaxEntries)

		// A single abusive /64 still hits its 100-request burst cap.
		for i := 0; i < trigger.PerIPBurst; i++ {
			require.True(t, lim.AllowClientIP("2001:db8:dead:beef::1"))
		}
		assert.False(t, lim.AllowClientIP("2001:db8:dead:beef::99"))
	})

	t.Run("instantaneous >10,000 /64 burst fails open on per-IP only while keyed endpoint/project tiers stay fail-closed", func(t *testing.T) {
		now := time.Date(2026, 11, 17, 2, 0, 0, 0, time.UTC)
		lim := trigger.NewIngressLimiter(trigger.LimiterConfig{
			Now: func() time.Time { return now },
		})

		// Fill all 10,000 per-IP slots at elapsed == 0 so zero buckets have refilled.
		for i := 0; i < trigger.LimiterMaxEntries; i++ {
			require.True(t, lim.AllowClientIP(fmt.Sprintf("2001:db8:beef:%04x::1", i)))
		}
		assert.Equal(t, trigger.LimiterMaxEntries, lim.PerIPLen())

		// Tracked abusive /64 still enforces its burst limit.
		for i := 1; i < trigger.PerIPBurst; i++ {
			require.True(t, lim.AllowClientIP("2001:db8:beef:0000::2"))
		}
		assert.False(t, lim.AllowClientIP("2001:db8:beef:0000::3"))

		// Brand-new legitimate sender fails open on per-IP without growing the map past MaxEntries.
		assert.True(t, lim.AllowClientIP("203.0.113.88"))
		assert.True(t, lim.AllowClientIP("2001:db8:aaaa:1::1"))
		assert.Equal(t, trigger.LimiterMaxEntries, lim.PerIPLen())

		// Keyed per-endpoint and per-project maps stay fail-closed while unrefilled,
		// and evict idle >= burst/rate (6s) rather than waiting 30m.
		smallNow := now
		smallLim := trigger.NewIngressLimiter(trigger.LimiterConfig{
			MaxEntries:              8,
			PerEndpointRate:         10,
			PerEndpointBurst:        60, // burst/rate = 6s
			PerProjectVerifiedRate:  50,
			PerProjectVerifiedBurst: 300, // burst/rate = 6s
			Now:                     func() time.Time { return smallNow },
		})
		for i := 0; i < 8; i++ {
			// Spend all burst tokens on each of the 8 keys so only idle >= burst/rate can refill them.
			epKey := fmt.Sprintf("ep-%d", i)
			projKey := fmt.Sprintf("proj-%d", i)
			for j := 0; j < 60; j++ {
				require.True(t, smallLim.AllowEndpoint(epKey))
			}
			for j := 0; j < 300; j++ {
				require.True(t, smallLim.AllowProjectVerified(projKey))
			}
		}
		// Still unrefilled at t+5s (< 6s): 9th endpoint and 9th project fail closed.
		smallNow = now.Add(5 * time.Second)
		assert.False(t, smallLim.AllowEndpoint("ep-overflow"))
		assert.False(t, smallLim.AllowProjectVerified("proj-overflow"))

		// At t+6s (== burst/rate, well below 30m MaxAge): refilled buckets are evicted and new keys succeed.
		smallNow = now.Add(6 * time.Second)
		assert.True(t, smallLim.AllowEndpoint("ep-overflow"))
		assert.True(t, smallLim.AllowProjectVerified("proj-overflow"))
	})
}

type stubCacheStore struct {
	store.TriggerStore
	endpoints map[string]*store.WebhookEndpoint
	getCalls  int
	getErr    error
	flushed   map[string]flushedCounter
	flushErr  error
}

type flushedCounter struct {
	receive int
	reject  int
	lastAt  *time.Time
}

func (s *stubCacheStore) GetWebhookEndpoint(_ context.Context, id string) (*store.WebhookEndpoint, error) {
	s.getCalls++
	if s.getErr != nil {
		return nil, s.getErr
	}
	ep, ok := s.endpoints[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *ep
	return &cp, nil
}

func (s *stubCacheStore) FlushWebhookEndpointCounters(_ context.Context, id string, receiveDelta, rejectDelta int, lastReceivedAt *time.Time) error {
	if s.flushErr != nil {
		return s.flushErr
	}
	if _, ok := s.endpoints[id]; !ok {
		return store.ErrNotFound
	}
	if s.flushed == nil {
		s.flushed = make(map[string]flushedCounter)
	}
	cur := s.flushed[id]
	cur.receive += receiveDelta
	cur.reject += rejectDelta
	if lastReceivedAt != nil && (cur.lastAt == nil || lastReceivedAt.After(*cur.lastAt)) {
		cp := *lastReceivedAt
		cur.lastAt = &cp
	}
	s.flushed[id] = cur
	return nil
}

func TestEndpointCache_PositiveNegativeSecretAndBounds(t *testing.T) {
	now := time.Date(2026, 11, 16, 16, 0, 0, 0, time.UTC)
	epID := uuid.NewString()
	st := &stubCacheStore{
		endpoints: map[string]*store.WebhookEndpoint{
			epID: {
				ID:            epID,
				ProjectID:     "proj-1",
				Name:          "deploy-hook",
				SecretVersion: 1,
				SignatureMode: store.WebhookSignatureModeScionV1,
				Status:        store.WebhookEndpointStatusActive,
			},
		},
	}
	cache := trigger.NewEndpointCache(trigger.EndpointCacheConfig{
		Now: func() time.Time { return now },
	})

	// 1. Positive cache hit within 5m TTL, refresh after 5m.
	got, err := cache.LookupEndpoint(context.Background(), st, epID)
	require.NoError(t, err)
	assert.Equal(t, epID, got.ID)
	assert.Equal(t, 1, st.getCalls)

	now = now.Add(4 * time.Minute)
	_, err = cache.LookupEndpoint(context.Background(), st, epID)
	require.NoError(t, err)
	assert.Equal(t, 1, st.getCalls, "cached endpoint must not hit store before 5m TTL")

	now = now.Add(2 * time.Minute) // 6m total > 5m TTL
	_, err = cache.LookupEndpoint(context.Background(), st, epID)
	require.NoError(t, err)
	assert.Equal(t, 2, st.getCalls, "expired positive cache entry must re-query store")

	// 2. Negative cache hit within 60s TTL, refresh after 60s; transient errors not negative-cached.
	missingID := uuid.NewString()
	_, err = cache.LookupEndpoint(context.Background(), st, missingID)
	require.ErrorIs(t, err, store.ErrNotFound)
	assert.Equal(t, 3, st.getCalls)

	now = now.Add(30 * time.Second)
	_, err = cache.LookupEndpoint(context.Background(), st, missingID)
	require.ErrorIs(t, err, store.ErrNotFound)
	assert.Equal(t, 3, st.getCalls, "unknown ID within 60s negative TTL must not hit store")

	now = now.Add(31 * time.Second) // 61s > 60s TTL
	_, err = cache.LookupEndpoint(context.Background(), st, missingID)
	require.ErrorIs(t, err, store.ErrNotFound)
	assert.Equal(t, 4, st.getCalls)

	transientID := uuid.NewString()
	st.getErr = errors.New("db timeout")
	_, err = cache.LookupEndpoint(context.Background(), st, transientID)
	require.Error(t, err)
	st.getErr = nil
	st.endpoints[transientID] = &store.WebhookEndpoint{ID: transientID, ProjectID: "proj-1", Status: store.WebhookEndpointStatusActive}
	gotTransient, err := cache.LookupEndpoint(context.Background(), st, transientID)
	require.NoError(t, err, "transient lookup failure must not be negative-cached")
	assert.Equal(t, transientID, gotTransient.ID)

	// 3. Negative cache bounded to 10,000 entries (§8.1 step 3).
	assert.Equal(t, 10000, trigger.MaxNegativeCacheEntries)
	for i := 0; i < trigger.MaxNegativeCacheEntries+100; i++ {
		_, err = cache.LookupEndpoint(context.Background(), st, fmt.Sprintf("missing-%06d", i))
		require.ErrorIs(t, err, store.ErrNotFound)
	}
	assert.Equal(t, trigger.MaxNegativeCacheEntries, cache.NegativeLen())

	// 4. Secret cache keyed by (endpointID, version) and invalidated by Invalidate.
	secretLoads := 0
	loader := func(_ context.Context, _, id string, version int) (string, error) {
		secretLoads++
		return fmt.Sprintf("sec-%s-v%d", id, version), nil
	}
	s1, err := cache.LookupSecret(context.Background(), loader, "proj-1", epID, 1)
	require.NoError(t, err)
	assert.Equal(t, "sec-"+epID+"-v1", s1)
	assert.Equal(t, 1, secretLoads)

	_, err = cache.LookupSecret(context.Background(), loader, "proj-1", epID, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, secretLoads)

	// Version bump misses cache immediately.
	s2, err := cache.LookupSecret(context.Background(), loader, "proj-1", epID, 2)
	require.NoError(t, err)
	assert.Equal(t, "sec-"+epID+"-v2", s2)
	assert.Equal(t, 2, secretLoads)

	// Invalidate evicts both secret versions and endpoint entry for epID.
	cache.Invalidate(epID)
	_, err = cache.LookupSecret(context.Background(), loader, "proj-1", epID, 2)
	require.NoError(t, err)
	assert.Equal(t, 3, secretLoads)
}

func TestCounterAccumulator_BatchRetryAndClose(t *testing.T) {
	ctx := context.Background()
	epID := uuid.NewString()
	deletedID := uuid.NewString()
	st := &stubCacheStore{
		endpoints: map[string]*store.WebhookEndpoint{
			epID: {ID: epID},
		},
	}
	acc := trigger.NewCounterAccumulator(time.Hour)

	t1 := time.Date(2026, 11, 16, 17, 0, 0, 0, time.UTC)
	t2 := t1.Add(5 * time.Second)
	acc.RecordAccepted(epID, t1)
	acc.RecordAccepted(epID, t2)
	acc.RecordRejected(epID)
	acc.RecordRejected(epID)
	acc.RecordRejected(epID)
	acc.RecordRejected(deletedID)
	assert.Empty(t, st.flushed, "recording counters must not write to storage before Flush")

	// Transient failure preserves deltas for the next Flush.
	st.flushErr = errors.New("temporary write error")
	require.Error(t, acc.Flush(ctx, st))
	assert.Equal(t, 2, acc.PendingLen())

	// Subsequent Flush succeeds and drops deletedID's ErrNotFound cleanly.
	st.flushErr = nil
	require.NoError(t, acc.Flush(ctx, st))
	assert.Zero(t, acc.PendingLen())
	require.Contains(t, st.flushed, epID)
	assert.Equal(t, 2, st.flushed[epID].receive)
	assert.Equal(t, 3, st.flushed[epID].reject)
	require.NotNil(t, st.flushed[epID].lastAt)
	assert.True(t, st.flushed[epID].lastAt.Equal(t2))

	// Close flushes once idempotently.
	acc.RecordAccepted(epID, t2.Add(time.Second))
	acc.Close(ctx, st)
	acc.Close(ctx, st)
	assert.Equal(t, 3, st.flushed[epID].receive)
}

func TestSlogAuditSink_ContentFreeAndSignatureFailureRateLimit(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	now := time.Date(2026, 11, 16, 18, 0, 0, 0, time.UTC)
	sink := trigger.NewSlogAuditSink(trigger.SlogAuditConfig{
		Logger:          logger,
		FailureInterval: time.Minute,
		Now:             func() time.Time { return now },
	})

	sink.RecordReceipt(context.Background(), trigger.ReceiptAuditRecord{
		ProjectID:    "proj-1",
		EndpointID:   "ep-1",
		EventID:      "ev-1",
		WebhookEvent: "deploy.finished",
		PayloadBytes: 128,
		Result:       trigger.IngressStatusQueued,
		RemoteIP:     "198.51.100.4",
	})

	// Emit 10 signature failures on ep-1 within 1 minute -> only 1 log line emitted.
	for i := 0; i < 10; i++ {
		sink.RecordSignatureFailure(context.Background(), trigger.SignatureFailureAuditRecord{
			ProjectID:  "proj-1",
			EndpointID: "ep-1",
			RemoteIP:   "198.51.100.9",
		})
	}
	// Another endpoint emits independently within the same minute.
	sink.RecordSignatureFailure(context.Background(), trigger.SignatureFailureAuditRecord{
		ProjectID:  "proj-1",
		EndpointID: "ep-2",
		RemoteIP:   "198.51.100.9",
	})

	// Advance 61s -> ep-1 emits again and reports the 9 suppressed failures.
	now = now.Add(61 * time.Second)
	sink.RecordSignatureFailure(context.Background(), trigger.SignatureFailureAuditRecord{
		ProjectID:  "proj-1",
		EndpointID: "ep-1",
		RemoteIP:   "198.51.100.9",
	})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 4, "expected 1 receipt + 1 initial ep-1 failure + 1 ep-2 failure + 1 post-window ep-1 failure")

	var receipt map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &receipt))
	assert.Equal(t, trigger.AuditEventReceived, receipt["event_type"])
	assert.Equal(t, "ev-1", receipt["event_id"])
	assert.Equal(t, "deploy.finished", receipt["webhook_event"])
	assert.Equal(t, float64(128), receipt["payload_bytes"])
	assert.Equal(t, "queued", receipt["result"])
	assert.NotContains(t, receipt, "payload")
	assert.NotContains(t, receipt, "secret")
	assert.NotContains(t, receipt, "headers")

	var postWindow map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[3]), &postWindow))
	assert.Equal(t, trigger.AuditEventSignatureFailed, postWindow["event_type"])
	assert.Equal(t, "ep-1", postWindow["endpoint_id"])
	assert.Equal(t, float64(9), postWindow["suppressed_count"])
}

func TestBoundedTables_ConstantWorkUnderFullTableFlood(t *testing.T) {
	const totalKeys = 50000 // N >> 10,000 (5x default 10,000 table capacity)
	ctx := context.Background()

	t.Run("per-IP limiter unrefilled flood and refilling /48 spray stay <= 8 probes/req", func(t *testing.T) {
		now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
		var maxSteps, evictCalls int
		lim := trigger.NewIngressLimiter(trigger.LimiterConfig{
			Now: func() time.Time { return now },
			OnEvictScan: func(steps int) {
				if steps > 0 {
					evictCalls++
				}
				if steps > maxSteps {
					maxSteps = steps
				}
			},
		})
		// Instantaneous 50,000-key flood at elapsed == 0 (0 buckets refilled -> worst-case probe).
		// After the table fills (LimiterMaxEntries), overflow keys are bounded by the shared
		// overflow bucket (PerIPOverflowBurst) rather than passing unlimited (SEC-2).
		allowed := 0
		for i := range totalKeys {
			ip := fmt.Sprintf("2001:db8:flood:%04x::1", i)
			if i >= 65536 {
				ip = fmt.Sprintf("2001:db8:flood2:%04x::1", i-65536)
			}
			if lim.AllowClientIP(ip) {
				allowed++
			}
		}
		assert.Equal(t, trigger.LimiterMaxEntries+trigger.PerIPOverflowBurst, allowed)
		assert.Equal(t, trigger.LimiterMaxEntries, lim.PerIPLen())
		assert.Equal(t, totalKeys-trigger.LimiterMaxEntries, evictCalls)
		assert.LessOrEqual(t, maxSteps, 8, "per-IP full-table probe must do O(1) bounded work per request")

		// Sustained 50,000-key spray advancing 180ms/req (~5.55 req/s): oldest bucket is always refilled -> 1 probe/req.
		maxSteps = 0
		evictCalls = 0
		limRefill := trigger.NewIngressLimiter(trigger.LimiterConfig{
			Now: func() time.Time { return now },
			OnEvictScan: func(steps int) {
				if steps > 0 {
					evictCalls++
				}
				if steps > maxSteps {
					maxSteps = steps
				}
			},
		})
		for i := range totalKeys {
			ip := fmt.Sprintf("198.18.%d.%d", (i>>8)&0xff, i&0xff)
			require.True(t, limRefill.AllowClientIP(ip))
			now = now.Add(180 * time.Millisecond)
		}
		assert.Equal(t, trigger.LimiterMaxEntries, limRefill.PerIPLen())
		assert.Equal(t, totalKeys-trigger.LimiterMaxEntries, evictCalls)
		assert.Equal(t, 1, maxSteps, "sustained spray with refilled head must evict in 1 step")
	})

	t.Run("per-endpoint and per-project-verified limiters stay <= 8 probes/req and clock-evict refilled entries behind exhausted ones", func(t *testing.T) {
		now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
		var maxSteps, evictCalls int
		lim := trigger.NewIngressLimiter(trigger.LimiterConfig{
			Now: func() time.Time { return now },
			OnEvictScan: func(steps int) {
				if steps > 0 {
					evictCalls++
				}
				if steps > maxSteps {
					maxSteps = steps
				}
			},
		})
		// Spend all 60 tokens on the first 20 endpoints (need 6s to refill), then 1 token on 9,980 endpoints (need 0.1s to refill).
		for i := range 20 {
			epID := fmt.Sprintf("ep-exhausted-%d", i)
			for range trigger.PerEndpointBurst {
				require.True(t, lim.AllowEndpoint(epID))
			}
		}
		for i := 20; i < trigger.LimiterMaxEntries; i++ {
			require.True(t, lim.AllowEndpoint(fmt.Sprintf("ep-light-%d", i)))
		}

		// At +200ms, the first 20 are still unrefilled while the 9,980 behind them ARE refilled.
		// Clock rotation advances past the 20 exhausted entries in <= 8 steps/req and admits new keys without dropping the 20 exhausted buckets.
		now = now.Add(200 * time.Millisecond)
		admitted := 0
		for i := range totalKeys - trigger.LimiterMaxEntries {
			if lim.AllowEndpoint(fmt.Sprintf("ep-new-%d", i)) {
				admitted++
			}
			now = now.Add(10 * time.Millisecond)
		}
		assert.Greater(t, admitted, totalKeys-trigger.LimiterMaxEntries-10)
		assert.Equal(t, totalKeys-trigger.LimiterMaxEntries, evictCalls)
		assert.LessOrEqual(t, maxSteps, 8)
	})

	t.Run("negative endpoint cache, positive endpoint cache, secret cache, and failure audit sink stay at 1 step/req", func(t *testing.T) {
		now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)

		// Negative endpoint cache (50,000 unknown IDs within 60s TTL -> 40,000 evictions, maxSteps == 1).
		var negMaxSteps, negEvicts int
		negCache := trigger.NewEndpointCache(trigger.EndpointCacheConfig{
			Now: func() time.Time { return now },
			OnEvictScan: func(steps int) {
				if steps > 0 {
					negEvicts++
				}
				if steps > negMaxSteps {
					negMaxSteps = steps
				}
			},
		})
		emptyStore := &stubCacheStore{endpoints: map[string]*store.WebhookEndpoint{}}
		for i := range totalKeys {
			_, err := negCache.LookupEndpoint(ctx, emptyStore, fmt.Sprintf("unknown-%06d", i))
			require.ErrorIs(t, err, store.ErrNotFound)
		}
		assert.Equal(t, trigger.MaxNegativeCacheEntries, negCache.NegativeLen())
		assert.Equal(t, totalKeys-trigger.MaxNegativeCacheEntries, negEvicts)
		assert.Equal(t, 1, negMaxSteps, "putUnknown must evict oldest in 1 step")

		// Positive endpoint cache + secret cache (50,000 active endpoints within 5m TTL -> 40,000 evictions each, maxSteps == 1).
		var posMaxSteps, posEvicts int
		posCache := trigger.NewEndpointCache(trigger.EndpointCacheConfig{
			Now: func() time.Time { return now },
			OnEvictScan: func(steps int) {
				if steps > 0 {
					posEvicts++
				}
				if steps > posMaxSteps {
					posMaxSteps = steps
				}
			},
		})
		posStore := &stubCacheStore{endpoints: make(map[string]*store.WebhookEndpoint, totalKeys)}
		for i := range totalKeys {
			id := fmt.Sprintf("ep-%06d", i)
			posStore.endpoints[id] = &store.WebhookEndpoint{
				ID:            id,
				ProjectID:     "proj-1",
				SecretVersion: 1,
				SignatureMode: store.WebhookSignatureModeScionV1,
				Status:        store.WebhookEndpointStatusActive,
			}
			_, err := posCache.LookupEndpoint(ctx, posStore, id)
			require.NoError(t, err)
			_, err = posCache.LookupSecret(ctx, func(_ context.Context, _, epID string, v int) (string, error) {
				return fmt.Sprintf("sec-%s-%d", epID, v), nil
			}, "proj-1", id, 1)
			require.NoError(t, err)
		}
		assert.Equal(t, trigger.MaxPositiveCacheEntries, posCache.PositiveLen())
		assert.Equal(t, trigger.MaxPositiveCacheEntries, posCache.SecretLen())
		assert.Equal(t, 2*(totalKeys-trigger.MaxPositiveCacheEntries), posEvicts)
		assert.Equal(t, 1, posMaxSteps, "putEndpoint and LookupSecret must evict oldest in 1 step")

		// Signature-failure audit sink (50,000 distinct endpoint IDs within 1m window -> 40,000 evictions, maxSteps == 1).
		var auditMaxSteps, auditEvicts int
		discardLogger := slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), &slog.HandlerOptions{Level: slog.LevelError}))
		sink := trigger.NewSlogAuditSink(trigger.SlogAuditConfig{
			Logger: loggerOrDiscard(discardLogger),
			Now:    func() time.Time { return now },
			OnEvictScan: func(steps int) {
				if steps > 0 {
					auditEvicts++
				}
				if steps > auditMaxSteps {
					auditMaxSteps = steps
				}
			},
		})
		for i := range totalKeys {
			sink.RecordSignatureFailure(ctx, trigger.SignatureFailureAuditRecord{
				ProjectID:  "proj-1",
				EndpointID: fmt.Sprintf("audit-ep-%06d", i),
				RemoteIP:   "198.51.100.1",
			})
		}
		assert.Equal(t, 10000, sink.FailureWindowLen())
		assert.Equal(t, totalKeys-10000, auditEvicts)
		assert.Equal(t, 1, auditMaxSteps, "RecordSignatureFailure must evict oldest window in 1 step")
	})
}

func loggerOrDiscard(l *slog.Logger) *slog.Logger { return l }

func BenchmarkBoundedEviction_FullTable(b *testing.B) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	b.Run("PerIPLimiter_FullUnrefilled", func(b *testing.B) {
		lim := trigger.NewIngressLimiter(trigger.LimiterConfig{
			Now: func() time.Time { return now },
		})
		for i := range trigger.LimiterMaxEntries {
			lim.AllowClientIP(fmt.Sprintf("198.18.%d.%d", (i>>8)&0xff, i&0xff))
		}
		b.ResetTimer()
		for i := range b.N {
			n := trigger.LimiterMaxEntries + i
			lim.AllowClientIP(fmt.Sprintf("198.19.%d.%d", (n>>8)&0xff, n&0xff))
		}
	})
	b.Run("NegativeCache_FullUnexpired", func(b *testing.B) {
		ctx := context.Background()
		cache := trigger.NewEndpointCache(trigger.EndpointCacheConfig{
			Now: func() time.Time { return now },
		})
		st := &stubCacheStore{endpoints: map[string]*store.WebhookEndpoint{}}
		for i := range trigger.MaxNegativeCacheEntries {
			_, _ = cache.LookupEndpoint(ctx, st, strconv.Itoa(i))
		}
		b.ResetTimer()
		for i := range b.N {
			_, _ = cache.LookupEndpoint(ctx, st, strconv.Itoa(trigger.MaxNegativeCacheEntries+i))
		}
	})
}

type noCounterTxStore struct {
	store.Store // deliberately does NOT implement store.DurableQueuePendingCounter
}

func (s *noCounterTxStore) LockTriggerQuota(_ context.Context, _ string) error { return nil }

func (s *noCounterTxStore) GetTriggerEventByDedupKey(_ context.Context, _, _, _ string) (*store.TriggerEvent, error) {
	return nil, store.ErrNotFound
}

type outerStoreWithQueueFallbackTrap struct {
	store.TriggerStore
	store.DurableQueueStore
	ep              *store.WebhookEndpoint
	outerCountCalls int
}

func (s *outerStoreWithQueueFallbackTrap) GetWebhookEndpoint(_ context.Context, id string) (*store.WebhookEndpoint, error) {
	if s.ep != nil && s.ep.ID == id {
		cp := *s.ep
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (s *outerStoreWithQueueFallbackTrap) CountDurableQueuePending(_ context.Context, _, _ string) (int64, error) {
	s.outerCountCalls++
	return 0, nil
}

func (s *outerStoreWithQueueFallbackTrap) DurableQueueStats(_ context.Context, _, _ string, _ time.Time) (store.DurableQueueStats, error) {
	s.outerCountCalls++
	return store.DurableQueueStats{}, nil
}

func (s *outerStoreWithQueueFallbackTrap) WithTx(_ context.Context, fn func(tx store.Store) error) error {
	return fn(&noCounterTxStore{})
}

func TestIngress_TxWithoutQueuePendingCounterFailsClosed500(t *testing.T) {
	now := time.Date(2026, 11, 17, 11, 0, 0, 0, time.UTC)
	epID := uuid.NewString()
	const secret = "dHgtbm8tZmFsbGJhY2stdGVzdC1zZWNyZXQtMzJieXRlcyE"
	outer := &outerStoreWithQueueFallbackTrap{
		ep: &store.WebhookEndpoint{
			ID:            epID,
			ProjectID:     uuid.NewString(),
			Name:          "no-fallback-ep",
			SecretVersion: 1,
			SignatureMode: store.WebhookSignatureModeScionV1,
			Status:        store.WebhookEndpointStatusActive,
		},
	}
	q := &fakeEnqueueQueue{}
	ing := trigger.NewIngress(trigger.IngressConfig{
		Store:      outer,
		QueueStore: outer,
		Queue:      q,
		LoadSecret: func(_ context.Context, _, _ string, _ int) (string, error) { return secret, nil },
		Enabled:    func() bool { return true },
		Now:        func() time.Time { return now },
	})

	payload := []byte(`{"test":1}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+epID, bytes.NewReader(payload))
	req.RemoteAddr = "198.51.100.10:4000"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(trigger.HeaderDelivery, "deliv-no-fallback")
	req.Header.Set(trigger.HeaderSignature, trigger.SignScionV1(secret, now.Unix(), "deliv-no-fallback", "", "", payload))
	rec := httptest.NewRecorder()
	ing.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.Zero(t, outer.outerCountCalls, "ingress must never fall back to non-transactional QueueStore")
	assert.Empty(t, q.enqueued)
	assert.Empty(t, q.wakes)
}

type flushNotifyStore struct {
	store.TriggerStore
	store.DurableQueueStore
	flushes chan struct{}
}

func (s *flushNotifyStore) WithTx(_ context.Context, _ func(tx store.Store) error) error {
	return nil
}

func (s *flushNotifyStore) FlushWebhookEndpointCounters(_ context.Context, _ string, _, _ int, _ *time.Time) error {
	select {
	case s.flushes <- struct{}{}:
	default:
	}
	return nil
}

func TestIngress_NoBackgroundCleanupGoroutinesOrTickersOnBoundedTables(t *testing.T) {
	// 1. Reflection guard: IngressLimiter exposes neither Cleanup nor StartCleanup.
	limType := reflect.TypeFor[*trigger.IngressLimiter]()
	_, hasCleanup := limType.MethodByName("Cleanup")
	_, hasStartCleanup := limType.MethodByName("StartCleanup")
	assert.False(t, hasCleanup, "IngressLimiter must not expose a full-table Cleanup method")
	assert.False(t, hasStartCleanup, "IngressLimiter must not spawn a StartCleanup ticker goroutine")

	// 2. AST guard over all non-test .go files in pkg/hub/trigger:
	//    Zero goroutines, tickers, timers, or AfterFunc calls exist anywhere on
	//    limiter, boundedTable, endpoint/negative/secret caches, or audit sink;
	//    the sole background ticker in the package is CounterAccumulator.StartLoop.
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var goLocations, timerLocations []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fileAST, parseErr := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		require.NoError(t, parseErr)
		for _, decl := range fileAST.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			fnLabel := name + ":" + fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				var recvBuf bytes.Buffer
				ast.Inspect(fn.Recv.List[0].Type, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok {
						recvBuf.WriteString(id.Name)
					}
					return true
				})
				fnLabel = name + ":" + recvBuf.String() + "." + fn.Name.Name
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.GoStmt:
					goLocations = append(goLocations, fnLabel)
				case *ast.CallExpr:
					if sel, ok := node.Fun.(*ast.SelectorExpr); ok {
						if pkgIdent, ok := sel.X.(*ast.Ident); ok && pkgIdent.Name == "time" {
							switch sel.Sel.Name {
							case "NewTicker", "Tick", "NewTimer", "After", "AfterFunc":
								timerLocations = append(timerLocations, fnLabel+"->time."+sel.Sel.Name)
							}
						}
					}
				}
				return true
			})
		}
	}
	assert.Equal(t, []string{"cache.go:CounterAccumulator.StartLoop"}, goLocations,
		"CounterAccumulator.StartLoop must be the only background goroutine in pkg/hub/trigger")
	assert.Equal(t, []string{"cache.go:CounterAccumulator.StartLoop->time.NewTicker"}, timerLocations,
		"CounterAccumulator.StartLoop must be the only ticker/timer/AfterFunc in pkg/hub/trigger")

	// 3. Runtime guard: populate all bounded tables, then start Ingress background
	//    loop and verify zero boundedTable operations occur from background ticks.
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	var scanHooksEnabled atomic.Bool
	var bgTableTouches atomic.Int64
	onScan := func(_ int) {
		if scanHooksEnabled.Load() {
			bgTableTouches.Add(1)
		}
	}

	lim := trigger.NewIngressLimiter(trigger.LimiterConfig{
		MaxEntries:  64,
		Now:         func() time.Time { return now },
		OnEvictScan: onScan,
	})
	cache := trigger.NewEndpointCache(trigger.EndpointCacheConfig{
		MaxPositiveEntries: 64,
		MaxNegativeEntries: 64,
		Now:                func() time.Time { return now },
		OnEvictScan:        onScan,
	})
	discardLogger := slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), &slog.HandlerOptions{Level: slog.LevelError}))
	sink := trigger.NewSlogAuditSink(trigger.SlogAuditConfig{
		Logger:      discardLogger,
		MaxEntries:  64,
		Now:         func() time.Time { return now },
		OnEvictScan: onScan,
	})
	populateCtx := context.Background()
	cacheBacking := &stubCacheStore{endpoints: make(map[string]*store.WebhookEndpoint, 64)}
	for i := range 64 {
		epID := fmt.Sprintf("ep-%03d", i)
		cacheBacking.endpoints[epID] = &store.WebhookEndpoint{
			ID:            epID,
			ProjectID:     "proj-1",
			SecretVersion: 1,
			SignatureMode: store.WebhookSignatureModeScionV1,
			Status:        store.WebhookEndpointStatusActive,
		}
		require.True(t, lim.AllowClientIP(fmt.Sprintf("198.51.100.%d", i)))
		require.True(t, lim.AllowEndpoint(epID))
		require.True(t, lim.AllowProjectVerified(fmt.Sprintf("proj-%d", i)))
		_, err := cache.LookupEndpoint(populateCtx, cacheBacking, epID)
		require.NoError(t, err)
		_, err = cache.LookupEndpoint(populateCtx, cacheBacking, fmt.Sprintf("missing-%03d", i))
		require.ErrorIs(t, err, store.ErrNotFound)
		_, err = cache.LookupSecret(populateCtx, func(_ context.Context, _, _ string, _ int) (string, error) {
			return "sec", nil
		}, "proj-1", epID, 1)
		require.NoError(t, err)
		sink.RecordSignatureFailure(populateCtx, trigger.SignatureFailureAuditRecord{
			ProjectID:  "proj-1",
			EndpointID: epID,
			RemoteIP:   "198.51.100.1",
		})
	}

	counters := trigger.NewCounterAccumulator(5 * time.Millisecond)
	notifyStore := &flushNotifyStore{flushes: make(chan struct{}, 8)}
	ing := trigger.NewIngress(trigger.IngressConfig{
		Store:      notifyStore,
		QueueStore: notifyStore,
		Queue:      &fakeEnqueueQueue{},
		Limiter:    lim,
		Cache:      cache,
		Counters:   counters,
		Audit:      sink,
		Logger:     discardLogger,
		Now:        func() time.Time { return now },
	})

	scanHooksEnabled.Store(true)
	bgCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ing.StartBackground(bgCtx)

	for range 3 {
		counters.RecordRejected("ep-001")
		select {
		case <-notifyStore.flushes:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for CounterAccumulator background flush")
		}
	}
	cancel()

	assert.Zero(t, bgTableTouches.Load(), "background loop must never scan or touch any bounded table")
}

func TestIngressLimiter_PerIPOverflowBucketBoundsFullTable(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	lim := trigger.NewIngressLimiter(trigger.LimiterConfig{
		PerIPRate:          0.001,
		PerIPBurst:         2,
		PerIPOverflowRate:  2, // 2 req/s refill on the shared overflow bucket
		PerIPOverflowBurst: 3, // at most 3 un-tracked keys allowed immediately
		MaxEntries:         4,
		Now:                func() time.Time { return now },
	})

	// Fill all 4 per-IP slots so none are refilled.
	for i := range 4 {
		require.True(t, lim.AllowClientIP(fmt.Sprintf("2001:db8:cafe:%04x::1", i)))
	}
	assert.Equal(t, 4, lim.PerIPLen())

	// New keys outside the full table are admitted through the shared overflow
	// bucket (not locked out fail-closed), up to PerIPOverflowBurst = 3, then
	// rate-limited rather than passing unlimited (SEC-2).
	for i := range 3 {
		assert.True(t, lim.AllowClientIP(fmt.Sprintf("2001:db8:dead:%04x::1", i)), "overflow request %d must be allowed", i)
	}
	assert.False(t, lim.AllowClientIP("2001:db8:dead:9999::1"), "4th overflow request must be bounded by the shared overflow bucket")
	assert.Equal(t, 4, lim.PerIPLen(), "table size must stay bounded at MaxEntries")

	// Tracked key still enforces its own per-IP burst independently of the overflow bucket.
	assert.True(t, lim.AllowClientIP("2001:db8:cafe:0000::2"))
	assert.False(t, lim.AllowClientIP("2001:db8:cafe:0000::3"))

	// Advancing clock by 500ms refills 1 overflow token (2/s * 0.5s = 1).
	now = now.Add(500 * time.Millisecond)
	assert.True(t, lim.AllowClientIP("203.0.113.42"))
	assert.False(t, lim.AllowClientIP("203.0.113.43"))
}

type countingIngressStore struct {
	*stubCacheStore
	getCalls atomic.Int64
}

func (s *countingIngressStore) GetWebhookEndpoint(ctx context.Context, id string) (*store.WebhookEndpoint, error) {
	s.getCalls.Add(1)
	return s.stubCacheStore.GetWebhookEndpoint(ctx, id)
}

func (s *countingIngressStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return errors.New("unexpected WithTx call")
}

func TestIngress_CacheMissChargesRejectionBeforeDBLookup(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	cachedActiveID := uuid.NewString()
	cachedDisabledID := uuid.NewString()
	coldActiveID := uuid.NewString()
	unknownRepeatedID := uuid.NewString()

	backing := &countingIngressStore{
		stubCacheStore: &stubCacheStore{
			endpoints: map[string]*store.WebhookEndpoint{
				cachedActiveID: {
					ID:            cachedActiveID,
					ProjectID:     "proj-1",
					SecretVersion: 1,
					SignatureMode: store.WebhookSignatureModeScionV1,
					Status:        store.WebhookEndpointStatusActive,
				},
				cachedDisabledID: {
					ID:            cachedDisabledID,
					ProjectID:     "proj-1",
					SecretVersion: 1,
					SignatureMode: store.WebhookSignatureModeScionV1,
					Status:        store.WebhookEndpointStatusDisabled,
				},
				coldActiveID: {
					ID:            coldActiveID,
					ProjectID:     "proj-1",
					SecretVersion: 1,
					SignatureMode: store.WebhookSignatureModeScionV1,
					Status:        store.WebhookEndpointStatusActive,
				},
			},
		},
	}

	cache := trigger.NewEndpointCache(trigger.EndpointCacheConfig{
		Now: func() time.Time { return now },
	})
	// Pre-warm cachedActiveID and cachedDisabledID.
	_, err := cache.LookupEndpoint(context.Background(), backing, cachedActiveID)
	require.NoError(t, err)
	_, err = cache.LookupEndpoint(context.Background(), backing, cachedDisabledID)
	require.NoError(t, err)
	backing.getCalls.Store(0)

	lim := trigger.NewIngressLimiter(trigger.LimiterConfig{
		PerIPRate:        100,
		PerIPBurst:       100,
		RejectionRate:    0.001, // effectively no refill
		RejectionBurst:   3,
		PerEndpointRate:  100,
		PerEndpointBurst: 100,
		Now:              func() time.Time { return now },
	})

	ing := trigger.NewIngress(trigger.IngressConfig{
		Store:      backing,
		QueueStore: &flushNotifyStore{},
		Queue:      &fakeEnqueueQueue{},
		LoadSecret: func(_ context.Context, _, _ string, _ int) (string, error) {
			return "test-secret", nil
		},
		Enabled: func() bool { return true },
		Limiter: lim,
		Cache:   cache,
		Logger:  slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), &slog.HandlerOptions{Level: slog.LevelError})),
		Now:     func() time.Time { return now },
	})

	postHook := func(id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+id, bytes.NewReader([]byte(`{"ok":true}`)))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "198.51.100.10:12345"
		rec := httptest.NewRecorder()
		ing.ServeHTTP(rec, req)
		return rec
	}

	// Token 1 of 3: cold unknown ID misses both caches -> charges 1 token BEFORE DB lookup,
	// hits DB once, caches in negative cache, returns 404 WITHOUT charging a second token.
	rec := postHook(unknownRepeatedID)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, int64(1), backing.getCalls.Load())

	// Token 2 of 3: same unknown ID hits negative cache -> 0 DB lookups, charges 1 token, returns 404.
	rec = postHook(unknownRepeatedID)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, int64(1), backing.getCalls.Load(), "negative cache hit must not query DB")

	// Token 3 of 3: cold active ID misses both caches -> charges 1 token BEFORE DB lookup,
	// hits DB once, caches active endpoint, proceeds to HMAC check (401 since unsigned).
	rec = postHook(coldActiveID)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, int64(2), backing.getCalls.Load())

	// Rejection bucket is now EMPTY (0 tokens left).
	// A brand-new unknown ID MUST return 429 WITHOUT calling GetWebhookEndpoint (SEC-1).
	rec = postHook(uuid.NewString())
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, int64(2), backing.getCalls.Load(), "unknown ID when rejection bucket is empty must not query DB")

	// A cached disabled endpoint also charges the rejection bucket and gets 429 without querying DB.
	rec = postHook(cachedDisabledID)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, int64(2), backing.getCalls.Load())

	// Cached active endpoints (both pre-warmed and newly-cached coldActiveID) are completely
	// unaffected by rejection-bucket exhaustion and never query DB.
	rec = postHook(cachedActiveID)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	rec = postHook(coldActiveID)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, int64(2), backing.getCalls.Load())
}
