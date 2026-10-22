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
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// stateToken returns a well-formed HS256-shaped agent credential.
func stateToken(t *testing.T, sub, jti string, iat, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims := map[string]any{"sub": sub, "iat": iat.Unix(), "exp": exp.Unix()}
	if jti != "" {
		claims["jti"] = jti
	}
	claimsJSON, _ := json.Marshal(claims)
	sig := make([]byte, 32)
	_, _ = rand.Read(sig)
	return header + "." + base64.RawURLEncoding.EncodeToString(claimsJSON) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// stateTestClient builds a client through the constructor, with no retries.
func stateTestClient(t *testing.T, url, token string) *Client {
	t.Helper()
	c := NewClientWithConfig(url, token, "agent-1")
	c.maxRetries = 0
	return c
}

// useStateDir points the published state at a fresh directory tree owned
// by the test user and returns the state directory.
func useStateDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "run", "scion")
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(SetCredentialStateDirForTest(dir, base))
	return dir
}

// hubJSONRefusal answers like the hub's agent-credential refusal.
func hubJSONRefusal(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = fmt.Fprint(w, `{"error":{"code":"unauthorized","message":"refused"}}`)
}

func TestCredentialState_RecordedAtConstruction(t *testing.T) {
	now := time.Now()
	c1 := stateToken(t, "agent-1", "jti-c1", now.Add(-time.Minute), now.Add(time.Hour))

	t.Run("NewClientWithConfig", func(t *testing.T) {
		c := NewClientWithConfig("http://127.0.0.1:1", c1, "agent-1")
		st := c.CredentialStateSnapshot()
		if st.Credential.JTI != "jti-c1" || st.Credential.FP != TokenFingerprint(c1).Short() {
			t.Fatalf("credential identity not recorded at construction: %+v", st.Credential)
		}
	})
	t.Run("NewClient", func(t *testing.T) {
		home := t.TempDir()
		t.Cleanup(SetTokenHome(home))
		if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(TokenFilePath(), []byte(c1), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(EnvHubEndpoint, "http://127.0.0.1:1")
		t.Setenv(EnvAgentID, "agent-1")
		c := NewClient()
		if c == nil {
			t.Fatal("NewClient returned nil")
		}
		st := c.CredentialStateSnapshot()
		if st.Credential.JTI != "jti-c1" || st.Credential.FP != TokenFingerprint(c1).Short() {
			t.Fatalf("credential identity not recorded at construction: %+v", st.Credential)
		}
	})
}

func TestCredentialState_EveryCredentialChangeRecorded(t *testing.T) {
	now := time.Now()
	c1 := stateToken(t, "agent-1", "jti-c1", now.Add(-time.Minute), now.Add(time.Hour))
	c2 := stateToken(t, "agent-1", "jti-c2", now, now.Add(time.Hour))
	c3 := stateToken(t, "agent-1", "jti-c3", now, now.Add(2*time.Hour))
	t.Cleanup(SetTokenHome(t.TempDir()))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(RefreshTokenResponse{Token: c3, ExpiresAt: now.Add(2 * time.Hour).UTC().Format(time.RFC3339)})
	}))
	defer srv.Close()
	c := stateTestClient(t, srv.URL, c1)
	check := func(want string) {
		t.Helper()
		got := c.CredentialStateSnapshot().Credential
		if got != CredentialIdentityOf(c.GetToken()) || got.JTI != want {
			t.Fatalf("state credential %+v, want %s (in memory %+v)", got, want, CredentialIdentityOf(c.GetToken()))
		}
	}
	check("jti-c1")
	c.SetToken(c2)
	check("jti-c2")
	if _, _, err := c.RefreshToken(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	check("jti-c3")
	r := c.CredentialStateSnapshot().Refresh
	if r == nil || r.Outcome != OutcomeRefreshed || r.FP != TokenFingerprint(c2).Short() {
		t.Fatalf("refresh record %+v, want refreshed with the presented credential's fp", r)
	}
}

func TestCredentialState_HeartbeatOutcomes(t *testing.T) {
	now := time.Now()
	c1 := stateToken(t, "agent-1", "jti-c1", now, now.Add(time.Hour))
	cases := []struct {
		name      string
		handler   func(w http.ResponseWriter)
		outcome   string
		status    int
		refusedBy string
	}{
		{"ok", func(w http.ResponseWriter) { w.WriteHeader(http.StatusOK) }, OutcomeSent, 200, ""},
		{"hub refusal", hubJSONRefusal, OutcomeRefused, 401, RefusedByHub},
		{"proxy refusal", func(w http.ResponseWriter) {
			w.Header().Set("X-Goog-IAP-Generated-Response", "true")
			w.WriteHeader(http.StatusUnauthorized)
		}, OutcomeRefused, 401, RefusedByProxy},
		{"non-JSON 403", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, "<html>denied</html>")
		}, OutcomeRefused, 403, RefusedByProxy},
		{"server error", func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) }, OutcomeFailed, 500, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tc.handler(w) }))
			defer srv.Close()
			c := stateTestClient(t, srv.URL, c1)
			_ = c.Heartbeat(context.Background())
			hb := c.CredentialStateSnapshot().Heartbeat
			if hb == nil || hb.Outcome != tc.outcome || hb.Status != tc.status || hb.RefusedBy != tc.refusedBy {
				t.Fatalf("heartbeat record %+v, want outcome=%s status=%d refused_by=%q", hb, tc.outcome, tc.status, tc.refusedBy)
			}
			if hb.FP != TokenFingerprint(c1).Short() {
				t.Fatalf("heartbeat fp %q, want the presented credential's", hb.FP)
			}
		})
	}
	t.Run("no answer", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		c := stateTestClient(t, url, c1)
		_ = c.Heartbeat(context.Background())
		hb := c.CredentialStateSnapshot().Heartbeat
		if hb == nil || hb.Outcome != OutcomeFailed || hb.Status != 0 {
			t.Fatalf("heartbeat record %+v, want failed with no status", hb)
		}
	})
}

// A paused heartbeat carries the fingerprint and source of the refused
// write that set the pause, not the credential in memory at the time.
func TestCredentialState_PausedRecordCarriesTheRefusal(t *testing.T) {
	now := time.Now()
	c1 := stateToken(t, "agent-1", "jti-c1", now, now.Add(time.Hour))
	c2 := stateToken(t, "agent-1", "jti-c2", now, now.Add(time.Hour))
	for _, tc := range []struct {
		name string
		h    func(w http.ResponseWriter)
		by   string
	}{
		{"hub", hubJSONRefusal, RefusedByHub},
		{"proxy", func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) }, RefusedByProxy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tc.h(w) }))
			defer srv.Close()
			c := stateTestClient(t, srv.URL, c1)
			c.writes.refreshRefused()
			_ = c.Heartbeat(context.Background()) // 401: sets the pause
			if err := c.Heartbeat(context.Background()); !errors.Is(err, ErrHubWritesPaused) {
				t.Fatalf("second heartbeat err=%v, want paused", err)
			}
			hb := c.CredentialStateSnapshot().Heartbeat
			if hb.Outcome != OutcomePaused || hb.FP != TokenFingerprint(c1).Short() || hb.RefusedBy != tc.by {
				t.Fatalf("paused record %+v, want fp of c1 and refused_by=%s", hb, tc.by)
			}

			// The credential changes but the gate is not reset yet (the
			// window between setTokenLocked and writes.reset in SetToken):
			// a paused heartbeat still names c1, never c2.
			c.tokenMu.Lock()
			c.setTokenLocked(c2)
			c.tokenMu.Unlock()
			_ = c.Heartbeat(context.Background())
			hb = c.CredentialStateSnapshot().Heartbeat
			if hb.Outcome != OutcomePaused || hb.FP != TokenFingerprint(c1).Short() {
				t.Fatalf("paused record after a credential change %+v, want fp of c1", hb)
			}
		})
	}
}

func TestCredentialState_RefreshOutcomes(t *testing.T) {
	now := time.Now()
	c1 := stateToken(t, "agent-1", "jti-c1", now, now.Add(time.Hour))
	t.Cleanup(SetTokenHome(t.TempDir()))
	for _, tc := range []struct {
		name    string
		h       func(w http.ResponseWriter)
		outcome string
		by      string
	}{
		{"hub refusal", hubJSONRefusal, OutcomeRefused, RefusedByHub},
		{"proxy refusal", func(w http.ResponseWriter) { w.WriteHeader(http.StatusForbidden) }, OutcomeRefused, RefusedByProxy},
		{"server error", func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) }, OutcomeFailed, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tc.h(w) }))
			defer srv.Close()
			c := stateTestClient(t, srv.URL, c1)
			_, _, _ = c.RefreshToken(context.Background())
			r := c.CredentialStateSnapshot().Refresh
			if r == nil || r.Outcome != tc.outcome || r.RefusedBy != tc.by || r.FP != TokenFingerprint(c1).Short() {
				t.Fatalf("refresh record %+v, want %s by %q with c1's fp", r, tc.outcome, tc.by)
			}
		})
	}
}

func TestCredentialState_OnlyTheCurrentCredentialCheckIsRecorded(t *testing.T) {
	now := time.Now()
	c1 := stateToken(t, "agent-1", "jti-c1", now, now.Add(time.Hour))
	cand := stateToken(t, "agent-1", "jti-cand", now, now.Add(time.Hour))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Scion-Agent-Token") == c1 {
			_, _ = fmt.Fprint(w, `{"id":"agent-1"}`)
			return
		}
		hubJSONRefusal(w)
	}))
	defer srv.Close()
	c := stateTestClient(t, srv.URL, c1)

	if err := c.CheckCredentialAccepted(context.Background(), cand); !errors.Is(err, ErrCandidateRefused) {
		t.Fatalf("candidate check err=%v", err)
	}
	if c.CredentialStateSnapshot().Check != nil {
		t.Fatal("a candidate check must not be recorded")
	}
	if err := c.CheckCurrentCredentialAccepted(context.Background(), c1); err != nil {
		t.Fatalf("current check: %v", err)
	}
	if ck := c.CredentialStateSnapshot().Check; ck == nil || ck.Outcome != OutcomeAccepted || ck.FP != TokenFingerprint(c1).Short() {
		t.Fatalf("check record %+v, want accepted for c1", ck)
	}
	c.SetToken(cand)
	_ = c.CheckCurrentCredentialAccepted(context.Background(), cand)
	if ck := c.CredentialStateSnapshot().Check; ck.Outcome != OutcomeRefused || ck.RefusedBy != RefusedByUnknown {
		t.Fatalf("check record %+v, want refused by unknown", ck)
	}
}

// The published state holds no value, no segment of it and no free text:
// only the expected keys.
func TestCredentialState_NoValueAndOnlyExpectedKeys(t *testing.T) {
	now := time.Now()
	c1 := stateToken(t, "agent-1", "jti-c1", now, now.Add(time.Hour))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"message":"free text from the hub"}}`)
	}))
	defer srv.Close()
	t.Cleanup(SetTokenHome(t.TempDir()))
	c := stateTestClient(t, srv.URL, c1)
	_ = c.Heartbeat(context.Background())
	_, _, _ = c.RefreshToken(context.Background())
	_ = c.CheckCurrentCredentialAccepted(context.Background(), c1)
	data, err := json.Marshal(c.CredentialStateSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if strings.Contains(s, c1) || strings.Contains(s, "free text") {
		t.Fatalf("state contains a value or response text: %s", s)
	}
	for _, seg := range strings.Split(c1, ".") {
		if strings.Contains(s, seg) {
			t.Fatalf("state contains a credential segment: %s", s)
		}
	}
	var top map[string]json.RawMessage
	_ = json.Unmarshal(data, &top)
	assertKeys(t, top, "version", "instance", "init_started_at", "updated_at", "seq", "credential", "heartbeat", "refresh", "check")
	var rec map[string]json.RawMessage
	_ = json.Unmarshal(top["heartbeat"], &rec)
	assertKeys(t, rec, "at", "outcome", "status", "fp", "refused_by")
	var cred map[string]json.RawMessage
	_ = json.Unmarshal(top["credential"], &cred)
	assertKeys(t, cred, "jti", "iat", "exp", "fp")
}

func sameIdentity(a, b CredentialIdentity) bool {
	return a.JTI == b.JTI && a.FP == b.FP && a.IssuedAt.Equal(b.IssuedAt) && a.Expiry.Equal(b.Expiry)
}

func assertKeys(t *testing.T, m map[string]json.RawMessage, allowed ...string) {
	t.Helper()
	ok := map[string]bool{}
	for _, k := range allowed {
		ok[k] = true
	}
	var keys []string
	bad := false
	for k := range m {
		keys = append(keys, k)
		if !ok[k] {
			bad = true
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 || bad {
		t.Errorf("keys %v, allowed %v", keys, allowed)
	}
}

func TestSafeJTI(t *testing.T) {
	for in, want := range map[string]string{
		"abc-123_X.y":           "abc-123_X.y",
		"":                      "",
		"has space":             "",
		"semi;colon":            "",
		strings.Repeat("a", 65): "",
		strings.Repeat("a", 64): strings.Repeat("a", 64),
	} {
		if got := safeJTI(in); got != want {
			t.Errorf("safeJTI(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestPublishCredentialState_SynchronousWriteAndRead(t *testing.T) {
	dir := useStateDir(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Left by an earlier run: a state and a temp file.
	if err := os.WriteFile(filepath.Join(dir, credentialStateFileName), []byte(`{"version":1,"instance":"old","seq":99}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, credentialStateTmpGlob+"stale"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c := stateTestClient(t, "http://127.0.0.1:1", stateToken(t, "agent-1", "jti-c1", now, now.Add(time.Hour)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !PublishCredentialState(ctx, c) {
		t.Fatal("PublishCredentialState returned false")
	}
	// Written before PublishCredentialState returned.
	st, status, reason := ReadCredentialState()
	if status != CredentialStateOK {
		t.Fatalf("status=%s reason=%s", status, reason)
	}
	if st.Instance == "" || st.Instance == "old" || st.Credential.JTI != "jti-c1" || st.InitStartedAt.IsZero() {
		t.Fatalf("published state %+v is not this run's", st)
	}
	if _, err := os.Stat(filepath.Join(dir, credentialStateTmpGlob+"stale")); !os.IsNotExist(err) {
		t.Fatalf("leftover temp file not removed: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, credentialStateFileName))
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("state file mode %v, err %v; want 0644", fi.Mode(), err)
	}
}

func TestPublishCredentialState_CreatesTheDirectory(t *testing.T) {
	useStateDir(t)
	now := time.Now()
	c := stateTestClient(t, "http://127.0.0.1:1", stateToken(t, "agent-1", "jti-c1", now, now.Add(time.Hour)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	PublishCredentialState(ctx, c)
	if _, status, reason := ReadCredentialState(); status != CredentialStateOK {
		t.Fatalf("status=%s reason=%s", status, reason)
	}
}

// Concurrent updates: the single writer leaves the file equal to the final
// state, and never writes an older snapshot over a newer one.
func TestCredentialStateWriter_LatestWins(t *testing.T) {
	useStateDir(t)
	now := time.Now()
	c := stateTestClient(t, "http://127.0.0.1:1", stateToken(t, "agent-1", "jti-c1", now, now.Add(time.Hour)))
	w := &credentialStateWriter{client: c}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				c.recordHeartbeat(writeResult{status: 200 + i, fp: "x"}, nil)
				if j%10 == 0 {
					c.SetToken(stateToken(t, "agent-1", fmt.Sprintf("jti-%d-%d", i, j), now, now.Add(time.Hour)))
				}
			}
		}(i)
	}
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-stop:
				return
			default:
				w.writeLatest()
			}
		}
	}()
	wg.Wait()
	close(stop)
	<-writerDone
	w.writeLatest()
	final := c.CredentialStateSnapshot()
	got, status, _ := ReadCredentialState()
	if status != CredentialStateOK {
		t.Fatalf("status=%s", status)
	}
	if got.Seq != final.Seq || !sameIdentity(got.Credential, final.Credential) ||
		got.Heartbeat.Status != final.Heartbeat.Status || !got.Heartbeat.At.Equal(final.Heartbeat.At) {
		t.Fatalf("file state seq=%d %+v, want final seq=%d %+v", got.Seq, got.Credential, final.Seq, final.Credential)
	}
	// An older snapshot is never written.
	w.lastSeq = final.Seq + 100
	c.recordHeartbeat(writeResult{status: 299}, nil)
	w.writeLatest()
	if again, _, _ := ReadCredentialState(); again.Seq != final.Seq {
		t.Fatalf("an older snapshot was written: seq %d", again.Seq)
	}
}

func TestCredentialStateWriter_FailedWriteRemovesTheFile(t *testing.T) {
	dir := useStateDir(t)
	now := time.Now()
	c := stateTestClient(t, "http://127.0.0.1:1", stateToken(t, "agent-1", "jti-c1", now, now.Add(time.Hour)))
	w := &credentialStateWriter{client: c}
	if !w.writeLatest() {
		t.Fatal("first write failed")
	}
	orig := writeCredentialStateFn
	writeCredentialStateFn = func(CredentialState) error { return errors.New("write failed for test") }
	t.Cleanup(func() { writeCredentialStateFn = orig })
	c.recordHeartbeat(writeResult{status: 401, refusedBy: RefusedByHub}, nil)
	if w.writeLatest() {
		t.Fatal("write should have failed")
	}
	if _, err := os.Stat(filepath.Join(dir, credentialStateFileName)); !os.IsNotExist(err) {
		t.Fatalf("state file left after a failed write: %v", err)
	}
}

// A client that is not published writes nothing; outside the test seam a
// test process never publishes.
func TestCredentialState_UnpublishedClientWritesNothing(t *testing.T) {
	now := time.Now()
	c := stateTestClient(t, "http://127.0.0.1:1", stateToken(t, "agent-1", "jti-c1", now, now.Add(time.Hour)))
	if PublishCredentialState(context.Background(), c) {
		t.Fatal("published without the test seam")
	}
	dir := useStateDir(t)
	c.recordHeartbeat(writeResult{status: 200}, nil)
	c.SetToken(stateToken(t, "agent-1", "jti-c2", now, now.Add(time.Hour)))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("an unpublished client created state: %v", err)
	}
}

func TestReadCredentialState_Refusals(t *testing.T) {
	now := time.Now()
	setup := func(t *testing.T) string {
		dir := useStateDir(t)
		c := stateTestClient(t, "http://127.0.0.1:1", stateToken(t, "agent-1", "jti-c1", now, now.Add(time.Hour)))
		if !(&credentialStateWriter{client: c}).writeLatest() {
			t.Fatal("write failed")
		}
		return dir
	}
	expect := func(t *testing.T, want string) {
		t.Helper()
		if _, status, reason := ReadCredentialState(); status != want {
			t.Fatalf("status=%s (%s), want %s", status, reason, want)
		}
	}
	t.Run("missing", func(t *testing.T) {
		useStateDir(t)
		expect(t, CredentialStateUnavailable)
	})
	t.Run("ok", func(t *testing.T) {
		setup(t)
		expect(t, CredentialStateOK)
	})
	t.Run("group-writable file", func(t *testing.T) {
		dir := setup(t)
		_ = os.Chmod(filepath.Join(dir, credentialStateFileName), 0o664)
		expect(t, CredentialStateUntrusted)
	})
	t.Run("group-writable directory", func(t *testing.T) {
		dir := setup(t)
		_ = os.Chmod(dir, 0o775)
		expect(t, CredentialStateUntrusted)
	})
	t.Run("other owner", func(t *testing.T) {
		setup(t)
		credentialStateOwner = os.Getuid() + 1
		expect(t, CredentialStateUntrusted)
	})
	t.Run("symlink leaf", func(t *testing.T) {
		dir := setup(t)
		p := filepath.Join(dir, credentialStateFileName)
		_ = os.Rename(p, p+".real")
		_ = os.Symlink(p+".real", p)
		expect(t, CredentialStateUntrusted)
	})
	t.Run("symlink directory", func(t *testing.T) {
		dir := setup(t)
		_ = os.Rename(dir, dir+".real")
		_ = os.Symlink(dir+".real", dir)
		expect(t, CredentialStateUntrusted)
	})
	t.Run("hard link", func(t *testing.T) {
		dir := setup(t)
		p := filepath.Join(dir, credentialStateFileName)
		_ = os.Link(p, filepath.Join(dir, "second"))
		expect(t, CredentialStateUntrusted)
	})
	t.Run("too large", func(t *testing.T) {
		dir := setup(t)
		_ = os.WriteFile(filepath.Join(dir, credentialStateFileName), []byte(strings.Repeat(" ", credentialStateMaxBytes+1)), 0o644)
		expect(t, CredentialStateUntrusted)
	})
	t.Run("not a state", func(t *testing.T) {
		dir := setup(t)
		_ = os.WriteFile(filepath.Join(dir, credentialStateFileName), []byte(`{"version":7}`), 0o644)
		expect(t, CredentialStateUntrusted)
	})
}

func TestClassifyRejection(t *testing.T) {
	mk := func(status int, hdr map[string]string) *http.Response {
		r := &http.Response{StatusCode: status, Header: http.Header{}}
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	cases := []struct {
		name string
		resp *http.Response
		body string
		want string
	}{
		{"nil", nil, "", RefusedByNone},
		{"200", mk(200, nil), "", RefusedByNone},
		{"hub 401", mk(401, nil), `{"error":{"code":"x"}}`, RefusedByHub},
		{"hub 403", mk(403, nil), `{"error":"x"}`, RefusedByHub},
		{"html 401", mk(401, nil), `<html>`, RefusedByProxy},
		{"iap 401 with json", mk(401, map[string]string{"X-Goog-IAP-Generated-Response": "true"}), `{"error":"x"}`, RefusedByProxy},
		{"sign-in redirect", mk(302, map[string]string{"Location": "https://accounts.google.com/o/oauth2"}), "", RefusedByProxy},
		{"other redirect", mk(302, map[string]string{"Location": "https://example.com/"}), "", RefusedByNone},
		{"500", mk(500, nil), `{"error":"x"}`, RefusedByNone},
	}
	for _, tc := range cases {
		if got := ClassifyRejection(tc.resp, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReadTokenFileWithModTime_WarnsOnOversizedFile(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(SetTokenHome(home))
	var warnings []string
	orig := warnTokenFile
	warnTokenFile = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { warnTokenFile = orig })
	_ = os.MkdirAll(filepath.Join(home, ".scion"), 0o700)
	if err := os.WriteFile(TokenFilePath(), []byte(strings.Repeat("a", tokenFileMaxBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ReadTokenFileWithModTime(); ok {
		t.Fatal("an oversized file was read")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], TokenFile) {
		t.Fatalf("warnings %q, want one naming the token file", warnings)
	}
}

func TestReadTokenFileWithModTime(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(SetTokenHome(home))
	if _, _, ok := ReadTokenFileWithModTime(); ok {
		t.Fatal("ok for a missing file")
	}
	_ = os.MkdirAll(filepath.Join(home, ".scion"), 0o700)
	if err := os.WriteFile(TokenFilePath(), []byte(" value \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-42 * time.Second).Truncate(time.Second)
	_ = os.Chtimes(TokenFilePath(), when, when)
	v, mod, ok := ReadTokenFileWithModTime()
	if !ok || v != "value" || !mod.Equal(when) {
		t.Fatalf("got %q %v %v, want value at %v", v, mod, ok, when)
	}
}
