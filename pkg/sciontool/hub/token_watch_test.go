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
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
)

// agentCredential builds an unsigned test credential: header alg, the
// given claims, and a signature of sigLen zero bytes.
func agentCredential(t *testing.T, alg string, claims map[string]any, sigLen int) string {
	t.Helper()
	h, err := json.Marshal(map[string]string{"alg": alg, "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(h) + "." + enc.EncodeToString(c) + "." + enc.EncodeToString(make([]byte, sigLen))
}

func validClaims(now time.Time, jti string) map[string]any {
	return map[string]any{
		"sub": "agent-1",
		"jti": jti,
		"iat": now.Unix(),
		"exp": now.Add(10 * time.Hour).Unix(),
	}
}

func TestValidateCandidateToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	valid := agentCredential(t, "HS256", validClaims(now, "id-1"), 32)
	expired := validClaims(now, "id-2")
	expired["exp"] = now.Add(-time.Second).Unix()
	noExp := validClaims(now, "id-3")
	delete(noExp, "exp")
	otherSub := validClaims(now, "id-4")
	otherSub["sub"] = "agent-2"

	tests := []struct {
		name  string
		token string
		want  error
	}{
		{"valid", valid, nil},
		{"empty", "", ErrCandidateEmpty},
		{"not a token", "dev-credential", ErrCandidateFormat},
		{"truncated signature", valid[:len(valid)-4], ErrCandidateSignature},
		{"empty signature", valid[:strings.LastIndex(valid, ".")+1], ErrCandidateSignature},
		{"cut before signature", valid[:strings.LastIndex(valid, ".")], ErrCandidateFormat},
		{"wrong algorithm", agentCredential(t, "none", validClaims(now, "id-5"), 32), ErrCandidateAlgorithm},
		{"garbage header", "%%%." + strings.SplitN(valid, ".", 2)[1], ErrCandidateHeader},
		{"garbage claims", strings.Split(valid, ".")[0] + ".%%%." + strings.Split(valid, ".")[2], ErrCandidateClaims},
		{"expired", agentCredential(t, "HS256", expired, 32), ErrCandidateExpired},
		{"no expiry", agentCredential(t, "HS256", noExp, 32), ErrCandidateNoExpiry},
		{"other agent", agentCredential(t, "HS256", otherSub, 32), ErrCandidateSubject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := ValidateCandidateToken(tt.token, "agent-1", now)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if err != nil {
				// Refusal reasons are fixed text and never quote the value.
				if tt.token != "" && strings.Contains(err.Error(), tt.token) {
					t.Fatalf("error quotes the credential")
				}
				return
			}
			if info.ID != "id-1" || !info.IssuedAt.Equal(now) || info.Fingerprint != TokenFingerprint(tt.token) {
				t.Fatalf("info = %+v", info)
			}
			if got := info.LogID(); got != "jti=id-1" {
				t.Fatalf("LogID = %q", got)
			}
		})
	}
}

func TestTokenInfoLogIDWithoutJTI(t *testing.T) {
	info := InspectToken("dev-credential")
	if !strings.HasPrefix(info.LogID(), "sha256:") || len(info.LogID()) != len("sha256:")+8 {
		t.Fatalf("LogID = %q", info.LogID())
	}
	if strings.Contains(info.LogID(), "dev-credential") {
		t.Fatal("LogID contains the value")
	}
}

func TestCredentialChanged(t *testing.T) {
	c := NewClientWithConfig("http://127.0.0.1:1", "old", "agent-1")
	ch := c.CredentialChanged()
	if again := c.CredentialChanged(); again != ch {
		t.Fatal("CredentialChanged returned a new channel without a change")
	}
	select {
	case <-ch:
		t.Fatal("closed before any change")
	default:
	}
	c.SetToken("new")
	select {
	case <-ch:
	default:
		t.Fatal("SetToken did not close the channel")
	}
	next := c.CredentialChanged()
	select {
	case <-next:
		t.Fatal("the fresh channel is already closed")
	default:
	}
	if (*Client)(nil).CredentialChanged() != nil {
		t.Fatal("nil client returned a channel")
	}
}

// fakeFiles stands in for the two watched files.
type fakeFiles struct {
	mu               sync.Mutex
	agent, transport string
	// onRead, if set, runs after every agent read (count is 1-based).
	reads  int
	onRead func(count int)
}

func (f *fakeFiles) readAgent() string {
	f.mu.Lock()
	f.reads++
	v, n, hook := f.agent, f.reads, f.onRead
	f.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return v
}

func (f *fakeFiles) readTransport() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.transport
}

func (f *fakeFiles) set(agent, transport string) {
	f.mu.Lock()
	f.agent, f.transport = agent, transport
	f.mu.Unlock()
}

type watchHarness struct {
	t      *testing.T
	clk    *clock.Fake
	files  *fakeFiles
	notify chan struct{}
	w      *TokenFileWatcher
	cancel context.CancelFunc
	done   <-chan struct{}
}

func newWatchHarness(t *testing.T, agent, transport string) *watchHarness {
	t.Helper()
	h := &watchHarness{
		t:      t,
		clk:    clock.NewFake(time.Unix(1_800_000_000, 0)),
		files:  &fakeFiles{agent: agent, transport: transport},
		notify: make(chan struct{}, 1),
	}
	h.w = NewTokenFileWatcher(TokenWatchConfig{
		Notify:        h.notify,
		Clock:         h.clk,
		readAgent:     h.files.readAgent,
		readTransport: h.files.readTransport,
	})
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = h.w.Run(ctx)
	t.Cleanup(func() {
		cancel()
		<-h.done
	})
	return h
}

// advanceTo waits until the watcher has armed a timer for at, then moves
// the clock there.
func (h *watchHarness) advanceTo(at time.Time) {
	h.t.Helper()
	if !h.clk.WaitForTimer(5*time.Second, at) {
		h.t.Fatalf("no timer armed for %s", at)
	}
	h.clk.Advance(at.Sub(h.clk.Now()))
}

// check runs one interval with no change seen and waits for the next
// interval timer, so the check has finished.
func (h *watchHarness) checkNoChange() {
	h.t.Helper()
	at := h.clk.Now().Add(DefaultTokenWatchInterval)
	h.advanceTo(at)
	if !h.clk.WaitForTimer(5*time.Second, at.Add(DefaultTokenWatchInterval)) {
		h.t.Fatal("watcher did not re-arm after a check")
	}
}

// checkChange runs one interval that sees a change, then the stability
// re-read, and waits for the next interval timer.
func (h *watchHarness) checkChange() {
	h.t.Helper()
	at := h.clk.Now().Add(DefaultTokenWatchInterval)
	h.advanceTo(at)
	stable := at.Add(DefaultTokenWatchStableDelay)
	h.advanceTo(stable)
	if !h.clk.WaitForTimer(5*time.Second, stable.Add(DefaultTokenWatchInterval)) {
		h.t.Fatal("watcher did not re-arm after the stability re-read")
	}
}

func (h *watchHarness) notified() bool {
	select {
	case <-h.notify:
		return true
	default:
		return false
	}
}

func TestTokenFileWatcher_SeededContentIsNotAChange(t *testing.T) {
	h := newWatchHarness(t, "credential-a", "transport-a")
	h.checkNoChange()
	h.checkNoChange()
	if h.notified() {
		t.Fatal("notified for the content present at start")
	}
}

func TestTokenFileWatcher_NotifiesOnStableAgentChange(t *testing.T) {
	h := newWatchHarness(t, "credential-a", "")
	h.files.set("credential-b", "")
	h.checkChange()
	if !h.notified() {
		t.Fatal("no notification for a new credential")
	}
	h.checkNoChange()
	if h.notified() {
		t.Fatal("notified twice for one change")
	}
}

func TestTokenFileWatcher_NotifiesOnTransportChange(t *testing.T) {
	h := newWatchHarness(t, "credential-a", "transport-a")
	h.files.set("credential-a", "transport-b")
	h.checkChange()
	if !h.notified() {
		t.Fatal("no notification for a new transport token")
	}
}

func TestTokenFileWatcher_EmptyFileIsNotAChange(t *testing.T) {
	h := newWatchHarness(t, "credential-a", "")
	h.files.set("", "")
	h.checkNoChange()
	if h.notified() {
		t.Fatal("notified for an emptied file")
	}
}

func TestTokenFileWatcher_UnstableFileWaitsForNextCheck(t *testing.T) {
	h := newWatchHarness(t, "credential-a", "")
	// The first read after the seed sees a partial value; the stability
	// re-read sees it completed.
	h.files.mu.Lock()
	seedReads := h.files.reads
	h.files.agent = "credential-b-partial"
	h.files.onRead = func(n int) {
		if n == seedReads+1 {
			h.files.mu.Lock()
			h.files.agent = "credential-b"
			h.files.mu.Unlock()
		}
	}
	h.files.mu.Unlock()
	h.checkChange()
	if h.notified() {
		t.Fatal("notified while the file was still changing")
	}
	h.checkChange()
	if !h.notified() {
		t.Fatal("no notification once the file was stable")
	}
}

func TestTokenFileWatcher_NotificationsCoalesce(t *testing.T) {
	h := newWatchHarness(t, "credential-a", "")
	h.files.set("credential-b", "")
	h.checkChange()
	h.files.set("credential-c", "")
	h.checkChange()
	if !h.notified() {
		t.Fatal("no notification")
	}
	if h.notified() {
		t.Fatal("two pending notifications; they should coalesce")
	}
}

func TestTokenFileWatcher_RefusedOtherValueIsReportedAgain(t *testing.T) {
	h := newWatchHarness(t, "credential-a", "")
	h.files.set("credential-b", "")
	h.checkChange()
	if !h.notified() {
		t.Fatal("no notification")
	}
	// The receiver read a different, invalid value (the file changed
	// after the report) and refused it; the file is now back to B.
	h.w.Refused(TokenFingerprint("credential-c-partial"))
	h.checkChange()
	if !h.notified() {
		t.Fatal("B was not reported again after a refusal of another value")
	}
}

func TestTokenFileWatcher_RefusedReportedValueIsNotRepeated(t *testing.T) {
	h := newWatchHarness(t, "credential-a", "")
	h.files.set("credential-bad", "")
	h.checkChange()
	if !h.notified() {
		t.Fatal("no notification")
	}
	h.w.Refused(TokenFingerprint("credential-bad"))
	h.checkNoChange()
	h.checkNoChange()
	if h.notified() {
		t.Fatal("a refused file was reported again")
	}
}

func TestTokenFileWatcher_StopsDuringStabilityDelay(t *testing.T) {
	h := newWatchHarness(t, "credential-a", "")
	h.files.set("credential-b", "")
	at := h.clk.Now().Add(DefaultTokenWatchInterval)
	h.advanceTo(at)
	if !h.clk.WaitForTimer(5*time.Second, at.Add(DefaultTokenWatchStableDelay)) {
		t.Fatal("no stability timer")
	}
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not stop during the stability delay")
	}
}

func TestTransportTokenFileChanged(t *testing.T) {
	t.Cleanup(SetTokenHome(t.TempDir()))
	c := NewClientWithConfig("http://127.0.0.1:1", "credential", "agent-1")
	c.oidcSource = NewTransportTokenFileSource()
	if c.TransportTokenFileChanged() {
		t.Fatal("changed with no file")
	}
	if err := WriteTransportTokenFile("transport-a", 0, 0); err != nil {
		t.Fatal(err)
	}
	c.NoteTransportTokenFile()
	if c.TransportTokenFileChanged() {
		t.Fatal("changed right after noting the file")
	}
	if err := WriteTransportTokenFile("transport-b", 0, 0); err != nil {
		t.Fatal(err)
	}
	if !c.TransportTokenFileChanged() {
		t.Fatal("a write by another process was not seen")
	}
	// A client without a hub-provided transport never reports a change.
	plain := NewClientWithConfig("http://127.0.0.1:1", "credential", "agent-1")
	if plain.TransportTokenFileChanged() {
		t.Fatal("changed for a client without a hub-provided transport")
	}
}
