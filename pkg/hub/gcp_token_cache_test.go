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

package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func makeTestJWT(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payloadBytes, _ := json.Marshal(map[string]int64{"exp": exp.Unix()})
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	return header + "." + payload + ".sig"
}

type countingTokenGenerator struct {
	accessCount int64
	idCount     int64
	email       string
	delay       time.Duration
	idTokenFn   func(ctx context.Context, sa, aud string) (*GCPIDToken, error)
}

func (g *countingTokenGenerator) GenerateAccessToken(_ context.Context, _ string, _ []string) (*GCPAccessToken, error) {
	atomic.AddInt64(&g.accessCount, 1)
	if g.delay > 0 {
		time.Sleep(g.delay)
	}
	return &GCPAccessToken{
		AccessToken: "ya29.cached-token",
		ExpiresIn:   3600,
		TokenType:   "Bearer",
	}, nil
}

func (g *countingTokenGenerator) GenerateIDToken(ctx context.Context, sa string, aud string) (*GCPIDToken, error) {
	atomic.AddInt64(&g.idCount, 1)
	if g.delay > 0 {
		time.Sleep(g.delay)
	}
	if g.idTokenFn != nil {
		return g.idTokenFn(ctx, sa, aud)
	}
	return &GCPIDToken{Token: "id-token"}, nil
}

func (g *countingTokenGenerator) VerifyImpersonation(_ context.Context, _ string) error {
	return nil
}

func (g *countingTokenGenerator) ServiceAccountEmail() string {
	return g.email
}

func TestCachedGCPTokenGenerator_CacheHit(t *testing.T) {
	inner := &countingTokenGenerator{email: "test@project.iam.gserviceaccount.com"}
	cached := NewCachedGCPTokenGenerator(inner)
	ctx := context.Background()

	// First call — cache miss
	tok1, err := cached.GenerateAccessToken(ctx, "sa@project.iam.gserviceaccount.com", []string{"https://www.googleapis.com/auth/cloud-platform"})
	if err != nil {
		t.Fatal(err)
	}
	if tok1.AccessToken != "ya29.cached-token" {
		t.Fatalf("unexpected token: %s", tok1.AccessToken)
	}

	// Second call — cache hit
	tok2, err := cached.GenerateAccessToken(ctx, "sa@project.iam.gserviceaccount.com", []string{"https://www.googleapis.com/auth/cloud-platform"})
	if err != nil {
		t.Fatal(err)
	}
	if tok2.AccessToken != "ya29.cached-token" {
		t.Fatalf("unexpected token: %s", tok2.AccessToken)
	}

	if atomic.LoadInt64(&inner.accessCount) != 1 {
		t.Fatalf("expected 1 inner call (cache hit), got %d", inner.accessCount)
	}
}

func TestCachedGCPTokenGenerator_DifferentSAs(t *testing.T) {
	inner := &countingTokenGenerator{email: "hub@project.iam.gserviceaccount.com"}
	cached := NewCachedGCPTokenGenerator(inner)
	ctx := context.Background()

	scopes := []string{"https://www.googleapis.com/auth/cloud-platform"}

	_, _ = cached.GenerateAccessToken(ctx, "sa1@project.iam.gserviceaccount.com", scopes)
	_, _ = cached.GenerateAccessToken(ctx, "sa2@project.iam.gserviceaccount.com", scopes)

	if atomic.LoadInt64(&inner.accessCount) != 2 {
		t.Fatalf("expected 2 inner calls (different SAs), got %d", inner.accessCount)
	}
}

func TestCachedGCPTokenGenerator_ScopeOrdering(t *testing.T) {
	inner := &countingTokenGenerator{email: "hub@project.iam.gserviceaccount.com"}
	cached := NewCachedGCPTokenGenerator(inner)
	ctx := context.Background()

	_, _ = cached.GenerateAccessToken(ctx, "sa@project.iam.gserviceaccount.com", []string{"scope-b", "scope-a"})
	_, _ = cached.GenerateAccessToken(ctx, "sa@project.iam.gserviceaccount.com", []string{"scope-a", "scope-b"})

	if atomic.LoadInt64(&inner.accessCount) != 1 {
		t.Fatalf("expected 1 inner call (same scopes, different order), got %d", inner.accessCount)
	}
}

func TestCachedGCPTokenGenerator_ConcurrentSingleflight(t *testing.T) {
	inner := &countingTokenGenerator{
		email: "hub@project.iam.gserviceaccount.com",
		delay: 200 * time.Millisecond,
	}
	cached := NewCachedGCPTokenGenerator(inner)
	ctx := context.Background()

	const concurrency = 20
	var wg sync.WaitGroup
	errs := make([]error, concurrency)

	for i := range concurrency {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = cached.GenerateAccessToken(ctx, "sa@project.iam.gserviceaccount.com",
				[]string{"https://www.googleapis.com/auth/cloud-platform"})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
	}

	count := atomic.LoadInt64(&inner.accessCount)
	if count != 1 {
		t.Fatalf("expected 1 inner call (singleflight), got %d", count)
	}
}

func TestCachedGCPTokenGenerator_IDTokenPassthrough(t *testing.T) {
	inner := &countingTokenGenerator{email: "hub@project.iam.gserviceaccount.com"}
	cached := NewCachedGCPTokenGenerator(inner)
	ctx := context.Background()

	tok, err := cached.GenerateIDToken(ctx, "sa@project.iam.gserviceaccount.com", "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "id-token" {
		t.Fatalf("unexpected id token: %s", tok.Token)
	}

	if atomic.LoadInt64(&inner.idCount) != 1 {
		t.Fatalf("expected 1 inner ID token call, got %d", inner.idCount)
	}
}

func TestCachedGCPTokenGenerator_IDTokenCacheHit(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	jwt := makeTestJWT(now.Add(1 * time.Hour))
	inner := &countingTokenGenerator{
		email: "hub@project.iam.gserviceaccount.com",
		idTokenFn: func(_ context.Context, _, _ string) (*GCPIDToken, error) {
			return &GCPIDToken{Token: jwt}, nil
		},
	}
	cached := NewCachedGCPTokenGenerator(inner)
	cached.now = func() time.Time { return now }

	ctx := context.Background()
	tok1, err := cached.GenerateIDToken(ctx, "sa@project.iam.gserviceaccount.com", "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	tok2, err := cached.GenerateIDToken(ctx, "sa@project.iam.gserviceaccount.com", "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	if tok1.Token != jwt || tok2.Token != jwt {
		t.Fatalf("unexpected tokens: %q, %q", tok1.Token, tok2.Token)
	}
	if got := atomic.LoadInt64(&inner.idCount); got != 1 {
		t.Fatalf("expected 1 inner ID token call (cache hit), got %d", got)
	}

	// Advance past the 5m short TTL -> must re-mint.
	now = now.Add(idTokenCacheTTL + time.Second)
	_, err = cached.GenerateIDToken(ctx, "sa@project.iam.gserviceaccount.com", "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&inner.idCount); got != 2 {
		t.Fatalf("expected 2 inner ID token calls after TTL expiry, got %d", got)
	}
}

func TestCachedGCPTokenGenerator_IDTokenDifferentKeys(t *testing.T) {
	inner := &countingTokenGenerator{
		email: "hub@project.iam.gserviceaccount.com",
		idTokenFn: func(_ context.Context, sa, aud string) (*GCPIDToken, error) {
			return &GCPIDToken{Token: sa + ":" + aud}, nil
		},
	}
	cached := NewCachedGCPTokenGenerator(inner)
	ctx := context.Background()

	t1, _ := cached.GenerateIDToken(ctx, "sa1@p.iam.gserviceaccount.com", "https://aud-a")
	t2, _ := cached.GenerateIDToken(ctx, "sa1@p.iam.gserviceaccount.com", "https://aud-b")
	t3, _ := cached.GenerateIDToken(ctx, "sa2@p.iam.gserviceaccount.com", "https://aud-a")
	t4, _ := cached.GenerateIDToken(ctx, "sa1@p.iam.gserviceaccount.com", "https://aud-a")

	if t1.Token != "sa1@p.iam.gserviceaccount.com:https://aud-a" ||
		t2.Token != "sa1@p.iam.gserviceaccount.com:https://aud-b" ||
		t3.Token != "sa2@p.iam.gserviceaccount.com:https://aud-a" ||
		t4.Token != t1.Token {
		t.Fatalf("unexpected tokens: %v %v %v %v", t1, t2, t3, t4)
	}
	if got := atomic.LoadInt64(&inner.idCount); got != 3 {
		t.Fatalf("expected 3 inner calls (3 distinct keys), got %d", got)
	}
}

func TestCachedGCPTokenGenerator_IDTokenJWTExpiryBound(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	// Token expires in 7m -> with 5m safety margin, cache expiry is 2m (< 5m TTL).
	shortJWT := makeTestJWT(now.Add(7 * time.Minute))
	inner := &countingTokenGenerator{
		email: "hub@project.iam.gserviceaccount.com",
		idTokenFn: func(_ context.Context, _, _ string) (*GCPIDToken, error) {
			return &GCPIDToken{Token: shortJWT}, nil
		},
	}
	cached := NewCachedGCPTokenGenerator(inner)
	cached.now = func() time.Time { return now }
	ctx := context.Background()

	_, err := cached.GenerateIDToken(ctx, "sa@p.iam.gserviceaccount.com", "https://aud")
	if err != nil {
		t.Fatal(err)
	}

	// At +1m, still within safe JWT expiry -> cache hit.
	now = now.Add(1 * time.Minute)
	_, err = cached.GenerateIDToken(ctx, "sa@p.iam.gserviceaccount.com", "https://aud")
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&inner.idCount); got != 1 {
		t.Fatalf("at +1m expected cache hit (1 call), got %d", got)
	}

	// At +2m01s, past jwtExp - 5m margin -> must re-mint.
	now = now.Add(1*time.Minute + time.Second)
	_, err = cached.GenerateIDToken(ctx, "sa@p.iam.gserviceaccount.com", "https://aud")
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&inner.idCount); got != 2 {
		t.Fatalf("at +2m01s expected re-mint (2 calls), got %d", got)
	}
}

func TestCachedGCPTokenGenerator_IDTokenMaxEntriesEviction(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	inner := &countingTokenGenerator{email: "hub@project.iam.gserviceaccount.com"}
	cached := NewCachedGCPTokenGenerator(inner)
	cached.maxIDCacheEntries = 3
	cached.now = func() time.Time { return now }
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		now = now.Add(time.Second)
		_, err := cached.GenerateIDToken(ctx, "sa@p.iam.gserviceaccount.com", fmt.Sprintf("https://aud-%d", i))
		if err != nil {
			t.Fatal(err)
		}
	}

	cached.idMu.RLock()
	size := len(cached.idCache)
	cached.idMu.RUnlock()
	if size != 3 {
		t.Fatalf("cache size = %d, want 3 (bounded)", size)
	}
}

func TestCachedGCPTokenGenerator_IDTokenConcurrentSingleflightAndLeaderCancel(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	inner := &countingTokenGenerator{
		email: "hub@project.iam.gserviceaccount.com",
		idTokenFn: func(ctx context.Context, _, aud string) (*GCPIDToken, error) {
			close(started)
			<-release
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return &GCPIDToken{Token: "tok-" + aud}, nil
		},
	}
	cached := NewCachedGCPTokenGenerator(inner)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	errs := make([]error, 5)
	toks := make([]*GCPIDToken, 5)

	wg.Add(1)
	go func() {
		defer wg.Done()
		toks[0], errs[0] = cached.GenerateIDToken(leaderCtx, "sa@p.iam.gserviceaccount.com", "https://aud")
	}()
	<-started

	for i := 1; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			toks[idx], errs[idx] = cached.GenerateIDToken(context.Background(), "sa@p.iam.gserviceaccount.com", "https://aud")
		}(i)
	}
	time.Sleep(20 * time.Millisecond)
	cancelLeader()
	close(release)
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("caller %d failed: %v", i, errs[i])
		}
		if toks[i].Token != "tok-https://aud" {
			t.Fatalf("caller %d got token %q", i, toks[i].Token)
		}
	}
	if got := atomic.LoadInt64(&inner.idCount); got != 1 {
		t.Fatalf("expected 1 inner call, got %d", got)
	}
}

func TestCachedGCPTokenGenerator_Passthrough(t *testing.T) {
	inner := &countingTokenGenerator{email: "hub@project.iam.gserviceaccount.com"}
	cached := NewCachedGCPTokenGenerator(inner)

	if cached.ServiceAccountEmail() != "hub@project.iam.gserviceaccount.com" {
		t.Fatalf("unexpected email: %s", cached.ServiceAccountEmail())
	}

	if err := cached.VerifyImpersonation(context.Background(), "sa@project.iam.gserviceaccount.com"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Compile-time check
var _ GCPTokenGenerator = (*CachedGCPTokenGenerator)(nil)
