package oidc

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/fakes"
)

const (
	aud   = "https://mediator.example.com"
	email = "cr-runner-scheduler-sa@example-project.iam.gserviceaccount.com"
)

func TestVerify(t *testing.T) {
	g := fakes.NewGoogle(fakes.Key(1))
	defer g.Close()
	now := time.Unix(1_700_000_000, 0)
	v := &Verifier{CertsURL: g.Server.URL, Audience: aud, Email: email, Now: func() time.Time { return now }}
	ctx := context.Background()

	base := func() map[string]any {
		return map[string]any{
			"iss": "https://accounts.google.com", "aud": aud, "email": email, "email_verified": true,
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		}
	}
	if err := v.Verify(ctx, g.IDToken(aud, email, now)); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if err := v.Verify(ctx, fakes.Mint(g.Key, g.Kid, base())); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}

	tests := []struct {
		name  string
		token string
	}{
		{"wrong audience", fakes.Mint(g.Key, g.Kid, with(base(), "aud", "https://other.example.com"))},
		{"wrong email", fakes.Mint(g.Key, g.Kid, with(base(), "email", "attacker@example-project.iam.gserviceaccount.com"))},
		{"unverified email", fakes.Mint(g.Key, g.Kid, with(base(), "email_verified", false))},
		{"wrong issuer", fakes.Mint(g.Key, g.Kid, with(base(), "iss", "https://evil.example.com"))},
		{"expired", fakes.Mint(g.Key, g.Kid, with(base(), "exp", now.Add(-2*time.Minute).Unix()))},
		{"future iat", fakes.Mint(g.Key, g.Kid, with(base(), "iat", now.Add(10*time.Minute).Unix()))},
		{"other signing key", fakes.Mint(fakes.Key(2), g.Kid, base())},
		{"unknown kid", fakes.Mint(g.Key, "other-kid", base())},
		{"malformed", "abc.def"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := v.Verify(ctx, tt.token); err == nil {
				t.Error("accepted")
			}
		})
	}
	if g.Fetches > 2 {
		t.Errorf("JWKS fetched %d times; unknown kids must not refetch more than once a minute", g.Fetches)
	}
}

func TestVerifyAudienceList(t *testing.T) {
	g := fakes.NewGoogle(fakes.Key(1))
	defer g.Close()
	now := time.Now()
	v := &Verifier{CertsURL: g.Server.URL, Audience: aud, Email: email}
	tok := fakes.Mint(g.Key, g.Kid, map[string]any{
		"iss": "accounts.google.com", "aud": []string{"x", aud}, "email": email, "email_verified": "true",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	if err := v.Verify(context.Background(), tok); err != nil {
		t.Errorf("aud list containing the audience rejected: %v", err)
	}
}

func TestVerifierRequiresConfig(t *testing.T) {
	v := &Verifier{}
	if err := v.Verify(context.Background(), "a.b.c"); err == nil {
		t.Error("verifier without audience accepted a token")
	}
}

func with(m map[string]any, k string, v any) map[string]any {
	m[k] = v
	return m
}

// TestJWKSOutageBackoff: with no key set and a failing JWKS endpoint,
// unauthenticated callers cannot drive more than one fetch a minute.
func TestJWKSOutageBackoff(t *testing.T) {
	g := fakes.NewGoogle(fakes.Key(1))
	defer g.Close()
	g.Fail = true
	now := time.Unix(1_700_000_000, 0)
	v := &Verifier{CertsURL: g.Server.URL, Audience: aud, Email: email, Now: func() time.Time { return now }}
	tok := g.IDToken(aud, email, now)
	for i := 0; i < 5; i++ {
		if err := v.Verify(context.Background(), tok); err == nil {
			t.Fatal("verified without keys")
		}
	}
	if g.Fetches != 1 {
		t.Errorf("JWKS fetched %d times during an outage, want 1", g.Fetches)
	}
	g.Fail = false
	now = now.Add(61 * time.Second)
	if err := v.Verify(context.Background(), g.IDToken(aud, email, now)); err != nil {
		t.Errorf("after the outage: %v", err)
	}
}

// TestFetchDoesNotBlockCachedKeys: while a JWKS fetch is in flight (here
// triggered by a token with an unknown kid, which anyone can send), a token
// signed with a cached, fresh key verifies without waiting for it.
func TestFetchDoesNotBlockCachedKeys(t *testing.T) {
	g := fakes.NewGoogle(fakes.Key(1))
	defer g.Close()
	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	v := &Verifier{CertsURL: g.Server.URL, Audience: aud, Email: email, Now: clock}
	ctx := context.Background()
	if err := v.Verify(ctx, g.IDToken(aud, email, clock())); err != nil {
		t.Fatalf("initial verify: %v", err)
	}
	mu.Lock()
	now = now.Add(61 * time.Second) // refetch allowed; keys still fresh (1 h)
	mu.Unlock()

	g.Started, g.Release = make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- v.Verify(ctx, fakes.Mint(g.Key, "other-kid", map[string]any{"iss": "accounts.google.com"}))
	}()
	<-g.Started // the fetch is in flight and blocked

	fast := make(chan error, 1)
	go func() { fast <- v.Verify(ctx, g.IDToken(aud, email, clock())) }()
	select {
	case err := <-fast:
		if err != nil {
			t.Errorf("cached-key verify failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("a cached-key verify waited for the in-flight JWKS fetch")
	}
	close(g.Release)
	if err := <-done; err == nil {
		t.Error("unknown kid verified")
	}
}
