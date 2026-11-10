package gcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/fakes"
)

func TestMetadataTokenCaching(t *testing.T) {
	g := fakes.NewGoogleAPIs("example-project", nil)
	defer g.Close()
	now := time.Unix(1_700_000_000, 0)
	ts := &MetadataTokenSource{BaseURL: g.Server.URL, Now: func() time.Time { return now }}
	ctx := context.Background()
	a, err := ts.Token(ctx)
	if err != nil || a == "" {
		t.Fatalf("Token = %q %v", a, err)
	}
	b, _ := ts.Token(ctx)
	if a != b || g.TokenFetches != 1 {
		t.Fatalf("token not cached: fetches = %d", g.TokenFetches)
	}
	now = now.Add(58 * time.Minute) // expires_in 3599 s: < 2 min left
	c, _ := ts.Token(ctx)
	if c == a || g.TokenFetches != 2 {
		t.Fatalf("token not refreshed near expiry: fetches = %d", g.TokenFetches)
	}
}

func TestSecretAccess(t *testing.T) {
	g := fakes.NewGoogleAPIs("example-project", map[string][]byte{"hook": []byte("FAKE-HOOK-SECRET")})
	defer g.Close()
	sm := &SecretManager{BaseURL: g.Server.URL, Tokens: &MetadataTokenSource{BaseURL: g.Server.URL}, Project: "example-project"}
	got, err := sm.Access(context.Background(), "hook")
	if err != nil || string(got) != "FAKE-HOOK-SECRET" {
		t.Fatalf("Access = %q %v", got, err)
	}
	_, err = sm.Access(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing secret: err = %v", err)
	}
	if strings.Contains(fmt.Sprint(err), "FAKE-HOOK-SECRET") {
		t.Error("error leaks a secret")
	}
}

func TestSecretChecksum(t *testing.T) {
	data := []byte("payload")
	good := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
	for _, tc := range []struct {
		crc    string
		wantOK bool
	}{{fmt.Sprint(good), true}, {fmt.Sprint(good + 1), false}, {"", true}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"payload":{"data":%q,"dataCrc32c":%q}}`, base64.StdEncoding.EncodeToString(data), tc.crc)
		}))
		sm := &SecretManager{BaseURL: srv.URL, Tokens: staticToken("t"), Project: "p"}
		_, err := sm.Access(context.Background(), "s")
		if (err == nil) != tc.wantOK {
			t.Errorf("crc %q: err = %v, wantOK %v", tc.crc, err, tc.wantOK)
		}
		srv.Close()
	}
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }
