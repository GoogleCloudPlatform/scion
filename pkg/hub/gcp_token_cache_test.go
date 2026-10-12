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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countingTokenGenerator struct {
	accessCount int64
	idCount     int64
	email       string
	delay       time.Duration
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

func (g *countingTokenGenerator) GenerateIDToken(_ context.Context, _ string, _ string) (*GCPIDToken, error) {
	atomic.AddInt64(&g.idCount, 1)
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
