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
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestPackageTrigger_DoesNotImportHub(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		f, parseErr := parser.ParseFile(fset, filepath.Clean(entry.Name()), nil, parser.ImportsOnly)
		require.NoError(t, parseErr)
		checked++
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			assert.NotEqual(t, "github.com/GoogleCloudPlatform/scion/pkg/hub", p,
				"%s must not import package hub (Stage 1 D-S1-1(c))", entry.Name())
		}
	}
	require.GreaterOrEqual(t, checked, 6)
}

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

	// Advance past MaxAge and run Cleanup; fresh burst applies again.
	now = now.Add(15 * time.Minute)
	lim.Cleanup(now)
	assert.True(t, lim.AllowClientIP("2001:db8:1:2::1"))
	assert.True(t, lim.AllowClientIP("2001:db8:1:2::2"))
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
