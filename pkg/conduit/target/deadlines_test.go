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

package target

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
)

// Stream authorization deadlines at the target (design v2.21.17 §3.5,
// "Target-side deadlines"; criterion 29).

const testInterval = time.Hour

var testBinding = Binding{SessionID: "sess-a", ConnectionEpoch: 4}

// closeRecorder records the closes of one stream.
type closeRecorder struct {
	mu    sync.Mutex
	codes []uint32
	why   []string
}

func (r *closeRecorder) close(code uint32, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.codes = append(r.codes, code)
	r.why = append(r.why, reason)
}

func (r *closeRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.codes)
}

type deadlineFixture struct {
	f     *fixture
	clk   *clock.Fake
	d     *Deadlines
	open  grant.Claims
	rec   *closeRecorder
	stop  func()
	cache *grant.MemoryReplayCache
}

// newDeadlineFixture tracks stream 3 on testBinding, opened at t0 with a
// deadline at t0 + testInterval.
func newDeadlineFixture(t *testing.T) *deadlineFixture {
	t.Helper()
	clk := clock.NewFake(t0)
	df := &deadlineFixture{f: newFixture(t), clk: clk, rec: &closeRecorder{}}
	df.cache = grant.NewMemoryReplayCache(clk.Now, 0)
	df.d = &Deadlines{Keys: df.f.keys, Replay: df.cache, Clock: clk}
	df.open = agentClaims(testBinding.ConnectionEpoch)
	df.open.Target.SessionID = testBinding.SessionID
	df.open.Subject = "user:u1"
	df.open.JTI = "open-jti-1"
	df.open.AuthzUntil = t0.Add(testInterval)
	df.open.AuthzInterval = testInterval
	df.stop = df.d.Track(testBinding, 3, &df.open, df.rec.close)
	t.Cleanup(df.stop)
	return df
}

// renewal returns a renewal for the tracked stream, minted now, moving the
// deadline to until.
func (df *deadlineFixture) renewal(t *testing.T, until time.Time, mut func(*grant.RenewalClaims)) []byte {
	t.Helper()
	now := df.clk.Now()
	c := grant.RenewalClaims{
		Issuer: df.open.Issuer, Subject: df.open.Subject, ProjectID: df.open.ProjectID,
		Target: df.open.Target, StreamID: 3, OpenJTI: df.open.JTI,
		AuthzUntil: until, NotBefore: now, Expiry: now.Add(30 * time.Second),
	}
	if mut != nil {
		mut(&c)
	}
	tok, err := grant.MintRenewal(df.f.signer, c)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (df *deadlineFixture) deadline(t *testing.T) time.Time {
	t.Helper()
	d, ok := df.d.Deadline(testBinding, 3)
	if !ok {
		t.Fatal("stream not tracked")
	}
	return d
}

// TestDeadlinesCloseAtDeadlinePlusGrace: with no renewal the stream is
// closed with 4401 authz_expired at authz_until + 30s, not before.
func TestDeadlinesCloseAtDeadlinePlusGrace(t *testing.T) {
	df := newDeadlineFixture(t)
	df.clk.Advance(testInterval + DeadlineGrace - time.Second)
	if n := df.rec.count(); n != 0 {
		t.Fatalf("closed %d times before authz_until + 30s", n)
	}
	df.clk.Advance(time.Second)
	if n := df.rec.count(); n != 1 {
		t.Fatalf("closes at authz_until + 30s = %d, want 1", n)
	}
	if df.rec.codes[0] != conduit.CloseUnauthenticated || df.rec.why[0] != ReasonAuthzExpired {
		t.Fatalf("close = %d %q, want 4401 authz_expired", df.rec.codes[0], df.rec.why[0])
	}
	if df.d.Len() != 0 {
		t.Fatal("expired stream still tracked")
	}
}

// TestDeadlinesRenewalExtends: a valid renewal moves the deadline and the
// close with it.
func TestDeadlinesRenewalExtends(t *testing.T) {
	df := newDeadlineFixture(t)
	df.clk.Advance(48 * time.Minute) // about 80% of the interval
	until := df.clk.Now().Add(testInterval)
	got, err := df.d.Renew(context.Background(), testBinding, 3, df.renewal(t, until, nil))
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if !got.Equal(until) || !df.deadline(t).Equal(until) {
		t.Fatalf("deadline = %v (returned %v), want %v", df.deadline(t), got, until)
	}
	// The old deadline passes without a close.
	df.clk.Advance(testInterval - 48*time.Minute + DeadlineGrace)
	if n := df.rec.count(); n != 0 {
		t.Fatalf("closed at the old deadline after a renewal (%d)", n)
	}
	df.clk.Advance(until.Add(DeadlineGrace).Sub(df.clk.Now()))
	if n := df.rec.count(); n != 1 {
		t.Fatalf("closes at the renewed deadline = %d, want 1", n)
	}
}

// TestDeadlinesRefusedRenewalsChangeNothing: a renewal bound to another
// stream_id or open_jti, a stream grant presented as a renewal, a replayed
// renewal, one that does not extend the deadline, one past the cap and
// one signed with an unknown kid each leave the deadline and the stream as
// they were.
func TestDeadlinesRefusedRenewalsChangeNothing(t *testing.T) {
	df := newDeadlineFixture(t)
	ctx := context.Background()
	df.clk.Advance(48 * time.Minute)
	now := df.clk.Now()
	before := df.deadline(t)

	replayed := df.renewal(t, now.Add(testInterval-time.Minute), nil)
	if _, err := df.d.Renew(ctx, testBinding, 3, replayed); err != nil {
		t.Fatalf("first use of the replayed renewal: %v", err)
	}
	before = df.deadline(t)

	openGrant, err := grant.Mint(df.f.signer, func() grant.Claims {
		c := df.open
		c.JTI = ""
		c.NotBefore, c.Expiry = now, now.Add(30*time.Second)
		c.AuthzUntil = now.Add(testInterval)
		return c
	}())
	if err != nil {
		t.Fatal(err)
	}
	otherKey := otherSigner(t)
	unknownKid, err := grant.MintRenewal(otherKey, grant.RenewalClaims{
		Issuer: df.open.Issuer, Subject: df.open.Subject, ProjectID: df.open.ProjectID,
		Target: df.open.Target, StreamID: 3, OpenJTI: df.open.JTI,
		AuthzUntil: now.Add(testInterval), NotBefore: now, Expiry: now.Add(30 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		stream uint32
		token  []byte
		want   error
	}{
		{"other stream_id", 3, df.renewal(t, now.Add(testInterval), func(c *grant.RenewalClaims) { c.StreamID = 5 }), grant.ErrStreamBinding},
		{"other open_jti", 3, df.renewal(t, now.Add(testInterval), func(c *grant.RenewalClaims) { c.OpenJTI = "open-jti-2" }), grant.ErrStreamBinding},
		{"stream grant as renewal", 3, openGrant, grant.ErrMalformed},
		{"replayed jti", 3, replayed, grant.ErrReplay},
		{"does not extend", 3, df.renewal(t, before, nil), grant.ErrAuthzUntil},
		{"past the cap", 3, df.renewal(t, now.Add(testInterval+DeadlineGrace+time.Second), nil), grant.ErrAuthzUntil},
		{"unknown kid", 3, unknownKid, grant.ErrUnknownKey},
		{"untracked stream", 9, df.renewal(t, now.Add(testInterval), func(c *grant.RenewalClaims) { c.StreamID = 9 }), ErrUntracked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := df.d.Renew(ctx, testBinding, tc.stream, tc.token); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got := df.deadline(t); !got.Equal(before) {
				t.Fatalf("deadline moved to %v (was %v)", got, before)
			}
			if n := df.rec.count(); n != 0 {
				t.Fatalf("stream closed by a refused renewal (%d)", n)
			}
		})
	}
}

// TestDeadlinesRenewalPastCapKeepsJTI: a renewal past now + authz_interval
// + 30s is refused without consuming its jti, so the same jti is still
// usable afterwards.
func TestDeadlinesRenewalPastCapKeepsJTI(t *testing.T) {
	df := newDeadlineFixture(t)
	ctx := context.Background()
	df.clk.Advance(48 * time.Minute)
	now := df.clk.Now()
	const jti = "renewal-jti-1"
	past := df.renewal(t, now.Add(testInterval+DeadlineGrace+time.Second), func(c *grant.RenewalClaims) { c.JTI = jti })
	if _, err := df.d.Renew(ctx, testBinding, 3, past); !errors.Is(err, grant.ErrAuthzUntil) {
		t.Fatalf("err = %v, want ErrAuthzUntil", err)
	}
	if got := df.deadline(t); !got.Equal(t0.Add(testInterval)) {
		t.Fatalf("deadline moved to %v", got)
	}
	within := df.renewal(t, now.Add(testInterval), func(c *grant.RenewalClaims) { c.JTI = jti })
	if _, err := df.d.Renew(ctx, testBinding, 3, within); err != nil {
		t.Fatalf("same jti after the refusal: %v", err)
	}
}

// TestDeadlinesOtherSession: a renewal arriving on another session (or
// epoch) does not reach the stream.
func TestDeadlinesOtherSession(t *testing.T) {
	df := newDeadlineFixture(t)
	tok := df.renewal(t, df.clk.Now().Add(testInterval), nil)
	for _, b := range []Binding{{SessionID: "sess-b", ConnectionEpoch: 4}, {SessionID: "sess-a", ConnectionEpoch: 5}} {
		if _, err := df.d.Renew(context.Background(), b, 3, tok); !errors.Is(err, ErrUntracked) {
			t.Fatalf("binding %+v: err = %v, want ErrUntracked", b, err)
		}
	}
}

// TestDeadlinesUntrackedWithoutDeadline: a stream whose grant carries no
// authz_until is not tracked and never closed.
func TestDeadlinesUntrackedWithoutDeadline(t *testing.T) {
	clk := clock.NewFake(t0)
	d := &Deadlines{Keys: &KeyHolder{}, Replay: grant.NewMemoryReplayCache(clk.Now, 0), Clock: clk}
	rec := &closeRecorder{}
	c := agentClaims(1)
	untrack := d.Track(testBinding, 3, &c, rec.close)
	defer untrack()
	if d.Len() != 0 {
		t.Fatal("stream without a deadline tracked")
	}
	clk.Advance(48 * time.Hour)
	if rec.count() != 0 {
		t.Fatal("stream without a deadline closed")
	}
}

// TestDeadlinesUntrackStopsClose: a stream that ended is no longer closed
// at its deadline.
func TestDeadlinesUntrackStopsClose(t *testing.T) {
	df := newDeadlineFixture(t)
	df.stop()
	df.clk.Advance(2 * testInterval)
	if df.rec.count() != 0 || df.d.Len() != 0 {
		t.Fatal("untracked stream closed or still tracked")
	}
}

// otherSigner returns a signer whose kid is not in any fixture key set.
func otherSigner(t *testing.T) *grant.Signer {
	t.Helper()
	k, err := grant.NewRingKey(t0.Add(-time.Hour), t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	r := grant.KeyRing{Keys: []grant.RingKey{k}}
	s, err := r.Signer(t0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestDeadlinesRenewalIssuerMustMatch: a renewal whose iss differs from
// the open grant's (or from Deadlines.Issuer when set) changes nothing.
func TestDeadlinesRenewalIssuerMustMatch(t *testing.T) {
	t.Run("open grant issuer", func(t *testing.T) {
		df := newDeadlineFixture(t)
		before := df.deadline(t)
		tok := df.renewal(t, df.clk.Now().Add(testInterval+10*time.Second), func(c *grant.RenewalClaims) { c.Issuer = "other-hub" })
		if _, err := df.d.Renew(context.Background(), testBinding, 3, tok); !errors.Is(err, grant.ErrIssuer) {
			t.Fatalf("err = %v, want ErrIssuer", err)
		}
		if got := df.deadline(t); !got.Equal(before) {
			t.Fatalf("deadline moved to %v", got)
		}
	})
	t.Run("configured issuer", func(t *testing.T) {
		df := newDeadlineFixture(t)
		df.d.Issuer = "configured-hub"
		tok := df.renewal(t, df.clk.Now().Add(testInterval+10*time.Second), nil) // iss = the open grant's
		if _, err := df.d.Renew(context.Background(), testBinding, 3, tok); !errors.Is(err, grant.ErrIssuer) {
			t.Fatalf("err = %v, want ErrIssuer", err)
		}
	})
}

// TestDeadlinesConcurrentRenewalsOneWins: two renewals to the same
// deadline arriving together: exactly one extends it, the other is
// refused with ErrAuthzUntil, and its jti is still unused.
func TestDeadlinesConcurrentRenewalsOneWins(t *testing.T) {
	df := newDeadlineFixture(t)
	until := df.clk.Now().Add(testInterval + 10*time.Second)
	a := df.renewal(t, until, func(c *grant.RenewalClaims) { c.JTI = "renewal-a" })
	b := df.renewal(t, until, func(c *grant.RenewalClaims) { c.JTI = "renewal-b" })
	errs := make(chan error, 2)
	start := make(chan struct{})
	for _, tok := range [][]byte{a, b} {
		go func(tok []byte) {
			<-start
			_, err := df.d.Renew(context.Background(), testBinding, 3, tok)
			errs <- err
		}(tok)
	}
	close(start)
	var ok, refused int
	for range 2 {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, grant.ErrAuthzUntil):
			refused++
		default:
			t.Fatalf("unexpected err %v", err)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("ok=%d refused=%d, want 1 and 1", ok, refused)
	}
	// Both jtis: one was consumed by the winner; the loser's is unused, so
	// exactly one of them is accepted again with a later deadline.
	later := until.Add(5 * time.Second)
	var reused int
	for _, jti := range []string{"renewal-a", "renewal-b"} {
		tok := df.renewal(t, later, func(c *grant.RenewalClaims) { c.JTI = jti })
		if _, err := df.d.Renew(context.Background(), testBinding, 3, tok); err == nil {
			reused++
			later = later.Add(5 * time.Second)
		} else if !errors.Is(err, grant.ErrReplay) {
			t.Fatalf("jti %s: err = %v, want nil or ErrReplay", jti, err)
		}
	}
	if reused != 1 {
		t.Fatalf("%d jtis reusable after the race, want 1 (the loser's)", reused)
	}
}
