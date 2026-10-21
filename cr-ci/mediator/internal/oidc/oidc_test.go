package oidc

import (
	"context"
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
