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

//go:build !no_sqlite

package artifacts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Ask for review (ptone/scion#5145 phase 2): the notify flow of
// POST /{id}/grants in the service. The hub's notifier, its subject checks
// and the notice row are tested in pkg/hub.

var (
	notifySubject   = principal{PrincipalKindUser, "user-sub", ""}
	notifySubjectID = PrincipalRef(notifySubject.kind, notifySubject.ref)
)

// recNotifier is a GrantNotifier that records its calls in log, shared
// with notifyLogStore so the order of the calls and the store write shows.
type recNotifier struct {
	mu        sync.Mutex
	log       *[]string
	deny      bool  // MayNotifyGrant answers (false, nil)
	mayErr    error // MayNotifyGrant answers (false, mayErr)
	notifyErr error // NotifyGrant fails
	notices   []GrantNotice
}

func (n *recNotifier) record(s string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	*n.log = append(*n.log, s)
}

func (n *recNotifier) MayNotifyGrant(_ context.Context, subjectUserID string) (bool, error) {
	n.record("may " + subjectUserID)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.mayErr != nil {
		return false, n.mayErr
	}
	return !n.deny, nil
}

func (n *recNotifier) NotifyGrant(_ context.Context, gn GrantNotice) error {
	n.record("notify " + gn.SubjectUserID + " " + gn.Permission)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.notifyErr != nil {
		return n.notifyErr
	}
	n.notices = append(n.notices, gn)
	return nil
}

func (n *recNotifier) sent() []GrantNotice {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]GrantNotice(nil), n.notices...)
}

// notifyHost is the fake host with a GrantNotifier.
type notifyHost struct {
	*fakeHost
	*recNotifier
}

// notifyLogStore records each PutGrantNotify (and PutGrant, which the
// grant route must no longer call) in the shared log, after the write.
type notifyLogStore struct {
	Store
	log *[]string
	mu  *sync.Mutex
}

func (s notifyLogStore) record(e string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.log = append(*s.log, e)
}

func (s notifyLogStore) PutGrant(ctx context.Context, g *Grant, max int, cross bool, actor *Actor) (GrantPut, error) {
	s.record("PutGrant")
	return s.Store.PutGrant(ctx, g, max, cross, actor)
}

func (s notifyLogStore) PutGrantNotify(ctx context.Context, g *Grant, max int, cross bool, actor *Actor, notify bool) (GrantPutResult, error) {
	res, err := s.Store.PutGrantNotify(ctx, g, max, cross, actor, notify)
	e := "put notify=false"
	if notify {
		e = "put notify=true"
	}
	if err != nil {
		e += " failed"
	}
	s.record(e)
	return res, err
}

// notifyFixture is newLinkFixture (userU owns artifact id, homed in
// project-1) over a host with a recording GrantNotifier, a recording
// store and a notify limiter of one token per requester, so a refusal
// that charged the limiter would show as rate_limited on the next request.
type notifyFixture struct {
	*fixture
	id    string
	n     *recNotifier
	calls *[]string
	now   *time.Time
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	f, id := newLinkFixture(t)
	calls := &[]string{}
	n := &recNotifier{log: calls}
	svc := NewService(&notifyHost{fakeHost: f.host, recNotifier: n})
	svc.SetStore(notifyLogStore{Store: f.store, log: calls, mu: &n.mu})
	svc.SetBlobStorage(f.blobs, "hub-1")
	now := time.Now()
	svc.notifyLimiter = newPeriodLimiter(func() time.Time { return now }, 1, time.Hour, 1, 100, time.Hour, 100, 100)
	f.svc = svc
	return &notifyFixture{fixture: f, id: id, n: n, calls: calls, now: &now}
}

// take returns and clears the recorded calls.
func (f *notifyFixture) take() []string {
	f.n.mu.Lock()
	defer f.n.mu.Unlock()
	out := *f.calls
	*f.calls = nil
	return out
}

// notifyGrant posts a grant with notify as p and decodes the response.
func (f *fixture) notifyGrant(p principal, id, kind, ref, perm string) (*httptest.ResponseRecorder, GrantResponse) {
	f.t.Helper()
	rec := f.postJSON(&p, grantsPath(id), GrantRequest{SubjectKind: kind, SubjectRef: ref, Permission: perm, Notify: true})
	var resp GrantResponse
	if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
		resp = decodeInto[GrantResponse](f.t, rec)
	}
	return rec, resp
}

// grantEvents returns the grant_created and grant_changed events of id
// naming subject.
func grantEvents(t *testing.T, st Store, id, subject string) []Event {
	t.Helper()
	var out []Event
	for _, e := range eventsOf(t, st, id) {
		if (e.Action == EventGrantCreated || e.Action == EventGrantChanged) && e.Detail["subjectRef"] == subject {
			out = append(out, e)
		}
	}
	return out
}

// lastNotified reports the "notified" detail of the latest grant event of
// id naming subject: (value, present).
func lastNotified(t *testing.T, st Store, id, subject string) (any, bool) {
	t.Helper()
	evs := grantEvents(t, st, id, subject)
	if len(evs) == 0 {
		t.Fatalf("no grant event for %s", subject)
	}
	v, ok := evs[len(evs)-1].Detail["notified"]
	return v, ok
}

func strs(ss []string) string { return strings.Join(ss, " | ") }

// TestGrantNotifyOrder (condition B2, spec amendment A1 C4): the send
// check runs before the store write and the limiter, and NotifyGrant only
// after a committed write recorded notified. On every refusal and fault
// path (not permitted, check fault, review read fault, review subject
// cannot read, not raised, grants unreadable, rate limited) the grant is
// made, the store is asked with notify=false, nothing is sent, and the
// event carries no notified; the refusals before the limiter do not charge
// it (the one-token limiter still lets the passing request through). A
// failed put never notifies (K12). No notifier: TestGrantNotifyWithoutNotifier.
func TestGrantNotifyOrder(t *testing.T) {
	f := newNotifyFixture(t)
	logs := captureLog(f.fixture, slog.LevelInfo)
	f.host.allow(notifySubject, "project-1", PermissionRead)

	// existing gives subject a grant of perm first, without notify.
	existing := func(t *testing.T, subject, perm string) {
		t.Helper()
		if rec, _ := f.putGrant(userU, f.id, SubjectPrincipal, subject, perm); rec.Code != http.StatusCreated {
			t.Fatalf("setup grant: %d %s", rec.Code, rec.Body.String())
		}
	}
	base := notifyLogStore{Store: f.store, log: f.calls, mu: &f.n.mu}
	refusals := []struct {
		name     string
		subject  string // "" = user:order-<name>
		set      func(t *testing.T, subject string)
		perm     string
		skipped  string
		reason   string // "" = no decision-log reason expected (not_raised)
		wantCode int
	}{
		{"not permitted", "", func(*testing.T, string) { f.n.deny = true },
			GrantRead, NotifySkippedNotPermitted, logReasonNotifyNotPermitted, http.StatusCreated},
		{"check fault", "", func(t *testing.T, sub string) { existing(t, sub, GrantRead); f.n.mayErr = errors.New("store down") },
			GrantWrite, NotifySkippedUnavailable, logReasonNotifyUnavailable, http.StatusOK},
		{"review subject read fault", notifySubjectID, func(*testing.T, string) { f.host.subjectErr = errors.New("lookup failed") },
			GrantReview, NotifySkippedUnavailable, logReasonNotifyUnavailable, http.StatusCreated},
		// The subject reads nothing in the home project (Q1).
		{"review subject cannot read", "user:order-unreadable", func(*testing.T, string) {},
			GrantReview, NotifySkippedNotPermitted, logReasonNotifyNotPermitted, http.StatusCreated},
		{"not raised", "", func(t *testing.T, sub string) { existing(t, sub, GrantWrite) },
			GrantRead, NotifySkippedNotRaised, "", http.StatusOK},
		// The raise pre-check cannot read the grants: the grant is still
		// made (condition H), with notify=false.
		{"grants unreadable", "", func(*testing.T, string) { f.svc.SetStore(listFailStore{notifyLogStore: base}) },
			GrantRead, NotifySkippedUnavailable, logReasonNotifyUnavailable, http.StatusCreated},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			f.n.deny, f.n.mayErr, f.host.subjectErr = false, nil, nil
			defer f.svc.SetStore(base)
			subject := tc.subject
			if subject == "" {
				subject = "user:order-" + strings.ReplaceAll(tc.name, " ", "-")
			}
			tc.set(t, subject)
			f.take()
			logs.take(t)
			rec, resp := f.notifyGrant(userU, f.id, SubjectPrincipal, subject, tc.perm)
			if rec.Code != tc.wantCode {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if resp.Notified || resp.NotifySkipped != tc.skipped {
				t.Errorf("response notified=%v skipped=%q, want false %q", resp.Notified, resp.NotifySkipped, tc.skipped)
			}
			// Every row writes the asked permission (not raised lowers
			// write to read).
			if g := f.grantOf(f.id, subject); g == nil || g.Permission != tc.perm {
				t.Errorf("grant not made as asked: %+v", g)
			}
			calls := f.take()
			want := []string{"may " + strings.TrimPrefix(subject, "user:"), "put notify=false"}
			if strs(calls) != strs(want) {
				t.Errorf("calls %q, want %q", calls, want)
			}
			if v, ok := lastNotified(t, f.store, f.id, subject); ok {
				t.Errorf("event notified = %v, want absent", v)
			}
			entries, _ := logs.take(t)
			found := tc.reason == ""
			for _, e := range entries {
				if e[logKeyReason] == tc.reason {
					found = true
				}
				if e[logKeyAction] == logActionGrantCreated || e[logKeyAction] == logActionGrantChanged {
					if e[logKeyNotified] != false {
						t.Errorf("grant entry notified = %v, want false", e[logKeyNotified])
					}
				}
			}
			if !found {
				t.Errorf("no decision log entry with reason %s: %v", tc.reason, entries)
			}
		})
	}
	f.n.deny, f.n.mayErr, f.host.subjectErr = false, nil, nil

	t.Run("passing request sends after the commit", func(t *testing.T) {
		f.take()
		rec, resp := f.notifyGrant(userU, f.id, SubjectPrincipal, "user:order-pass", GrantRead)
		if rec.Code != http.StatusCreated || !resp.Notified || resp.NotifySkipped != "" {
			t.Fatalf("status %d notified=%v skipped=%q: %s", rec.Code, resp.Notified, resp.NotifySkipped, rec.Body.String())
		}
		want := []string{"may order-pass", "put notify=true", "notify order-pass read"}
		if calls := f.take(); strs(calls) != strs(want) {
			t.Errorf("calls %q, want %q", calls, want)
		}
		if v, ok := lastNotified(t, f.store, f.id, "user:order-pass"); !ok || v != true {
			t.Errorf("event notified = %v %v, want true", v, ok)
		}
	})

	// The passing request spent the one token: refill it, so the failed
	// put below is asked with notify=true.
	t.Run("failed put never notifies", func(t *testing.T) {
		*f.now = f.now.Add(2 * time.Hour)
		f.take()
		other := f.publish(userU, "gone.md", []byte("# gone"), "scope=project-1").Artifact.ID
		f.svc.SetStore(failingPutStore{notifyLogStore: notifyLogStore{Store: f.store, log: f.calls, mu: &f.n.mu}})
		defer f.svc.SetStore(notifyLogStore{Store: f.store, log: f.calls, mu: &f.n.mu})
		rec, _ := f.notifyGrant(userU, other, SubjectPrincipal, "user:order-failed", GrantRead)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status %d, want 500: %s", rec.Code, rec.Body.String())
		}
		want := []string{"may order-failed", "put notify=true failed"}
		if calls := f.take(); strs(calls) != strs(want) {
			t.Errorf("calls %q, want %q (no NotifyGrant after a failed put)", calls, want)
		}
	})

	// The failed put spent the refilled token, so the next raise is rate
	// limited: the grant is still made, with notify=false.
	t.Run("rate limited", func(t *testing.T) {
		f.take()
		logs.take(t)
		rec, resp := f.notifyGrant(userU, f.id, SubjectPrincipal, "user:order-limited", GrantRead)
		if rec.Code != http.StatusCreated || resp.Notified || resp.NotifySkipped != NotifySkippedRateLimited {
			t.Fatalf("status %d notified=%v skipped=%q, want 201 false rate_limited", rec.Code, resp.Notified, resp.NotifySkipped)
		}
		if g := f.grantOf(f.id, "user:order-limited"); g == nil || g.Permission != GrantRead {
			t.Errorf("grant not made as asked: %+v", g)
		}
		want := []string{"may order-limited", "put notify=false"}
		if calls := f.take(); strs(calls) != strs(want) {
			t.Errorf("calls %q, want %q", calls, want)
		}
		if v, ok := lastNotified(t, f.store, f.id, "user:order-limited"); ok {
			t.Errorf("event notified = %v, want absent", v)
		}
		entries, _ := logs.take(t)
		found := false
		for _, e := range entries {
			found = found || e[logKeyReason] == logReasonNotifyRateLimited
		}
		if !found {
			t.Errorf("no decision log entry with reason %s", logReasonNotifyRateLimited)
		}
	})
}

// listFailStore fails every ListGrants call (the raise pre-check's read).
type listFailStore struct{ notifyLogStore }

func (listFailStore) ListGrants(context.Context, string) ([]Grant, error) {
	return nil, errors.New("grants unreadable")
}

// failingPutStore fails every grant write after recording it.
type failingPutStore struct{ notifyLogStore }

func (s failingPutStore) PutGrantNotify(_ context.Context, _ *Grant, _ int, _ bool, _ *Actor, notify bool) (GrantPutResult, error) {
	e := "put notify=false failed"
	if notify {
		e = "put notify=true failed"
	}
	s.record(e)
	return GrantPutResult{}, errors.New("disk full")
}

// TestGrantNotifyWithoutNotifier (condition H, K32n): a host without a
// GrantNotifier sends nothing: the grant is made and the response says
// unavailable, and the event carries no notified.
func TestGrantNotifyWithoutNotifier(t *testing.T) {
	f, id := newLinkFixture(t)
	rec, resp := f.notifyGrant(userU, id, SubjectPrincipal, notifySubjectID, GrantRead)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if resp.Notified || resp.NotifySkipped != NotifySkippedUnavailable {
		t.Errorf("notified=%v skipped=%q, want false %q", resp.Notified, resp.NotifySkipped, NotifySkippedUnavailable)
	}
	if g := f.grantOf(id, notifySubjectID); g == nil || g.Permission != GrantRead {
		t.Errorf("grant not made: %+v", g)
	}
	if v, ok := lastNotified(t, f.store, id, notifySubjectID); ok {
		t.Errorf("event notified = %v, want absent", v)
	}
}

// TestGrantNotifyRaise (v4 3.2, K5, K7, K13, K34n): a grant with notify
// sends only when it raises the user's level (created counts): a raise
// sends, a lowering or a no-op answers not_raised without charging the
// limiter (the one token left before the lowering still serves the final
// raise), a grant to the caller answers self, and a concurrent change that
// turns the raise into a lowering or a no-op under the lock sends nothing
// although the limiter token is spent.
func TestGrantNotifyRaise(t *testing.T) {
	f := newNotifyFixture(t)
	type step struct {
		name     string
		perm     string
		code     int
		notified bool
		skipped  string
		event    bool // a new grant event is written
		refill   bool // the requester's one token is refilled first
	}
	steps := []step{
		{"create", GrantRead, http.StatusCreated, true, "", true, true},
		{"raise to write", GrantWrite, http.StatusOK, true, "", true, true},
		{"lower to read", GrantRead, http.StatusOK, false, NotifySkippedNotRaised, true, true},
		{"no-op read", GrantRead, http.StatusOK, false, NotifySkippedNotRaised, false, false},
		// No refill: the token refilled before the lowering must still be
		// there, because neither the lowering nor the no-op charged it.
		{"raise to admin", GrantAdmin, http.StatusOK, true, "", true, false},
	}
	for _, st := range steps {
		if st.refill {
			*f.now = f.now.Add(2 * time.Hour)
		}
		before := len(grantEvents(t, f.store, f.id, notifySubjectID))
		sentBefore := len(f.n.sent())
		rec, resp := f.notifyGrant(userU, f.id, SubjectPrincipal, notifySubjectID, st.perm)
		if rec.Code != st.code {
			t.Fatalf("%s: status %d, want %d: %s", st.name, rec.Code, st.code, rec.Body.String())
		}
		if resp.Notified != st.notified || resp.NotifySkipped != st.skipped {
			t.Errorf("%s: notified=%v skipped=%q, want %v %q", st.name, resp.Notified, resp.NotifySkipped, st.notified, st.skipped)
		}
		evs := grantEvents(t, f.store, f.id, notifySubjectID)
		if got := len(evs) - before; (got == 1) != st.event {
			t.Fatalf("%s: %d new events", st.name, got)
		}
		if st.event {
			v, ok := evs[len(evs)-1].Detail["notified"]
			if st.notified && (!ok || v != true) {
				t.Errorf("%s: event notified = %v %v, want true", st.name, v, ok)
			}
			if !st.notified && ok {
				t.Errorf("%s: event notified = %v, want absent", st.name, v)
			}
		}
		sent := f.n.sent()
		if got := len(sent) - sentBefore; (got == 1) != st.notified {
			t.Errorf("%s: %d notices sent", st.name, got)
		} else if st.notified {
			n := sent[len(sent)-1]
			want := GrantNotice{ArtifactID: f.id, HomeScope: "project-1", SubjectUserID: notifySubject.ref, Permission: st.perm,
				ActorKind: PrincipalKindUser, ActorRef: userU.ref, Authority: AuthorityOwner}
			if n != want {
				t.Errorf("%s: notice %+v, want %+v", st.name, n, want)
			}
		}
	}

	t.Run("self", func(t *testing.T) {
		f.take()
		rec, resp := f.notifyGrant(userU, f.id, SubjectPrincipal, PrincipalRef(userU.kind, userU.ref), GrantRead)
		if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if resp.Notified || resp.NotifySkipped != NotifySkippedSelf {
			t.Errorf("notified=%v skipped=%q, want false self", resp.Notified, resp.NotifySkipped)
		}
		for _, c := range f.take() {
			if strings.HasPrefix(c, "may ") || strings.HasPrefix(c, "notify ") || c == "put notify=true" {
				t.Errorf("self grant reached %q", c)
			}
		}
	})

	for _, race := range []struct {
		name, held string
	}{
		{"raise becomes a lowering under the lock", GrantAdmin},
		{"raise becomes a no-op under the lock", GrantWrite},
	} {
		t.Run(race.name, func(t *testing.T) {
			subject := "user:race-" + race.held
			if rec, _ := f.putGrant(userU, f.id, SubjectPrincipal, subject, GrantRead); rec.Code != http.StatusCreated {
				t.Fatalf("setup: %d", rec.Code)
			}
			*f.now = f.now.Add(2 * time.Hour)
			f.svc.SetStore(raisingStore{Store: f.store, subject: subject, to: race.held})
			defer f.svc.SetStore(notifyLogStore{Store: f.store, log: f.calls, mu: &f.n.mu})
			sentBefore := len(f.n.sent())
			rec, resp := f.notifyGrant(userU, f.id, SubjectPrincipal, subject, GrantWrite)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if resp.Notified || resp.NotifySkipped != NotifySkippedNotRaised {
				t.Errorf("notified=%v skipped=%q, want false not_raised", resp.Notified, resp.NotifySkipped)
			}
			if len(f.n.sent()) != sentBefore {
				t.Errorf("a notice was sent")
			}
			for _, e := range grantEvents(t, f.store, f.id, subject) {
				if _, ok := e.Detail["notified"]; ok {
					t.Errorf("event %s carries notified: %v", e.Action, e.Detail)
				}
			}
			// The token was spent: the next raise by the same requester
			// within the hour is rate limited.
			f.svc.SetStore(notifyLogStore{Store: f.store, log: f.calls, mu: &f.n.mu})
			if _, resp := f.notifyGrant(userU, f.id, SubjectPrincipal, subject, GrantAdmin); resp.NotifySkipped != NotifySkippedRateLimited {
				t.Errorf("next raise: skipped=%q, want rate_limited (token spent)", resp.NotifySkipped)
			}
		})
	}
}

// raisingStore gives subject the level to with a separate write just
// before the grant route's write, as a concurrent administrator would.
type raisingStore struct {
	Store
	subject, to string
}

func (s raisingStore) PutGrantNotify(ctx context.Context, g *Grant, max int, cross bool, actor *Actor, notify bool) (GrantPutResult, error) {
	if g.SubjectRef == s.subject {
		if _, err := s.PutGrant(ctx, &Grant{ID: uuid.NewString(), ArtifactID: g.ArtifactID, SubjectKind: SubjectPrincipal,
			SubjectRef: s.subject, Permission: s.to, CreatedAt: time.Now()}, max, cross, testActor); err != nil {
			return GrantPutResult{}, err
		}
	}
	return s.Store.PutGrantNotify(ctx, g, max, cross, actor, notify)
}

// TestNotifyLimiter (condition H, K6): the notify limiter allows a burst of
// 10 per requester, then 30 per hour; requesters are independent; all
// requesters together get a burst of 60 and 600 per hour. A 24 h period
// works the same way (the ptone/scion#5548 reuse).
func TestNotifyLimiter(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	l := newPeriodLimiter(clock, NotifyPerHour, time.Hour, NotifyBurst, NotifyGlobalPerHour, time.Hour, NotifyGlobalBurst, notifyMaxRequesters)
	for i := 0; i < NotifyBurst; i++ {
		if _, ok := l.allow("user:a"); !ok {
			t.Fatalf("burst request %d refused", i+1)
		}
	}
	if _, ok := l.allow("user:a"); ok {
		t.Fatal("request past the burst allowed")
	}
	if _, ok := l.allow("user:b"); !ok {
		t.Fatal("another requester limited by the first")
	}
	// 30 per hour: one token every 2 minutes.
	now = now.Add(119 * time.Second)
	if _, ok := l.allow("user:a"); ok {
		t.Error("token before 2 minutes")
	}
	now = now.Add(time.Second)
	if _, ok := l.allow("user:a"); !ok {
		t.Error("no token after 2 minutes")
	}

	// The global burst of 60 bounds all requesters together.
	g := newPeriodLimiter(clock, NotifyPerHour, time.Hour, NotifyBurst, NotifyGlobalPerHour, time.Hour, NotifyGlobalBurst, notifyMaxRequesters)
	allowed := 0
	for r := 0; r < 10; r++ {
		for i := 0; i < NotifyBurst; i++ {
			if _, ok := g.allow("user:" + string(rune('a'+r))); ok {
				allowed++
			}
		}
	}
	if allowed != NotifyGlobalBurst {
		t.Errorf("global burst: %d allowed, want %d", allowed, NotifyGlobalBurst)
	}
	// 600 per hour globally: one token every 6 seconds.
	now = now.Add(6 * time.Second)
	if _, ok := g.allow("user:z"); !ok {
		t.Error("no global token after 6 seconds")
	}
	if _, ok := g.allow("user:y"); ok {
		t.Error("second global token within 6 seconds")
	}

	// A daily period.
	d := newPeriodLimiter(clock, 2, 24*time.Hour, 1, 100, 24*time.Hour, 100, 10)
	if _, ok := d.allow("agent:x"); !ok {
		t.Fatal("first daily request refused")
	}
	if _, ok := d.allow("agent:x"); ok {
		t.Fatal("second daily request within the burst of 1 allowed")
	}
	now = now.Add(12 * time.Hour)
	if _, ok := d.allow("agent:x"); !ok {
		t.Error("no daily token after 12 hours (2 per day)")
	}
}

// TestRateLimiterWrapperUnchanged (v4 O1): newRateLimiter, the share-link
// limiter, is newPeriodLimiter with per-minute periods and keeps its rates.
func TestRateLimiterWrapperUnchanged(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	a := newRateLimiter(clock, SharedClientPerMinute, SharedClientBurst, SharedGlobalPerMinute, SharedGlobalBurst, sharedMaxClients)
	b := newPeriodLimiter(clock, SharedClientPerMinute, time.Minute, SharedClientBurst, SharedGlobalPerMinute, time.Minute, SharedGlobalBurst, sharedMaxClients)
	if a.clientRate != float64(SharedClientPerMinute)/60 || a.globalRate != float64(SharedGlobalPerMinute)/60 ||
		a.clientBurst != SharedClientBurst || a.globalBurst != SharedGlobalBurst || a.maxClients != sharedMaxClients {
		t.Errorf("share-link limiter rates changed: %+v", a)
	}
	if a.clientRate != b.clientRate || a.globalRate != b.globalRate || a.clientBurst != b.clientBurst ||
		a.globalBurst != b.globalBurst || a.maxClients != b.maxClients {
		t.Errorf("wrapper differs from newPeriodLimiter(minute)")
	}
	z := newRateLimiter(clock, 0, 0, 0, 0, 0)
	if z.clientRate != 1.0/60 || z.clientBurst != 1 || z.globalRate != 1.0/60 || z.globalBurst != 1 || z.maxClients != 1 {
		t.Errorf("zero rates not floored at 1: %+v", z)
	}
}

// TestGrantNotifyAutomaticReviewGrant (#7134 overlap): asking the
// delegating user for review while the automatic review grant exists is
// not a raise (not_raised, nothing sent); raising them to write by a person
// sends, and #7134's creator hand-over still happens.
func TestGrantNotifyAutomaticReviewGrant(t *testing.T) {
	f, h := newSponsorReviewFixture(t)
	calls := &[]string{}
	n := &recNotifier{log: calls}
	svc := NewService(&sponsorNotifyHost{sponsorReviewHost: h, recNotifier: n})
	svc.SetStore(f.store)
	svc.SetBlobStorage(f.blobs, "hub-1")
	f.svc = svc
	id := f.publish(agentA, "a.md", []byte("# a"), "").Artifact.ID
	checkSponsorGrant(t, f, id)
	f.host.allow(srSteward, "project-1", PermissionRead)
	f.host.allowSteward(srSteward, agentA.ref, "project-1", StewardClauseProjectOwner)

	rec, resp := f.notifyGrant(srSteward, id, SubjectPrincipal, srSponsorID, GrantReview)
	if rec.Code != http.StatusOK {
		t.Fatalf("review over the automatic grant: %d %s", rec.Code, rec.Body.String())
	}
	if resp.Notified || resp.NotifySkipped != NotifySkippedNotRaised {
		t.Errorf("review over the automatic grant: notified=%v skipped=%q, want not_raised", resp.Notified, resp.NotifySkipped)
	}
	if g := f.grantOf(id, srSponsorID); g == nil || g.CreatedByRef != SponsorReviewCreator {
		t.Errorf("automatic grant changed: %+v", g)
	}
	if len(n.sent()) != 0 {
		t.Errorf("notice sent for a no-op")
	}

	rec, resp = f.notifyGrant(srSteward, id, SubjectPrincipal, srSponsorID, GrantWrite)
	if rec.Code != http.StatusOK || !resp.Notified || resp.NotifySkipped != "" {
		t.Fatalf("raise to write: %d notified=%v skipped=%q %s", rec.Code, resp.Notified, resp.NotifySkipped, rec.Body.String())
	}
	g := f.grantOf(id, srSponsorID)
	if g == nil || g.Permission != GrantWrite || g.CreatedByRef != PrincipalRef(srSteward.kind, srSteward.ref) {
		t.Errorf("after the raise: %+v, want write created by the steward (#7134 hand-over)", g)
	}
	if sent := n.sent(); len(sent) != 1 || sent[0].SubjectUserID != srSponsor.ref || sent[0].Permission != GrantWrite ||
		sent[0].ActorRef != srSteward.ref {
		t.Errorf("notices %+v", sent)
	}
}

// sponsorNotifyHost is the sponsor-review host with a GrantNotifier.
type sponsorNotifyHost struct {
	*sponsorReviewHost
	*recNotifier
}

// TestGrantNotifyReviewNeedsRead (section 2.5 step 5, Q1 default, K30n):
// until review reads (phase 3), a review grant with notify to a user who
// cannot read the artifact some other way is made (inert, as today) and
// answers not_permitted, sending nothing; to a home-project reader it
// sends.
func TestGrantNotifyReviewNeedsRead(t *testing.T) {
	f := newNotifyFixture(t)
	rec, resp := f.notifyGrant(userU, f.id, SubjectPrincipal, notifySubjectID, GrantReview)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if resp.Notified || resp.NotifySkipped != NotifySkippedNotPermitted {
		t.Errorf("non-reader: notified=%v skipped=%q, want not_permitted", resp.Notified, resp.NotifySkipped)
	}
	if g := f.grantOf(f.id, notifySubjectID); g == nil || g.Permission != GrantReview {
		t.Errorf("review grant not made: %+v", g)
	}
	if len(f.n.sent()) != 0 {
		t.Errorf("notice sent to a non-reader")
	}

	reader := principal{PrincipalKindUser, "user-home-reader", ""}
	f.host.allow(reader, "project-1", PermissionRead)
	rec, resp = f.notifyGrant(userU, f.id, SubjectPrincipal, PrincipalRef(reader.kind, reader.ref), GrantReview)
	if rec.Code != http.StatusCreated || !resp.Notified {
		t.Fatalf("home reader: %d notified=%v skipped=%q", rec.Code, resp.Notified, resp.NotifySkipped)
	}
	if sent := f.n.sent(); len(sent) != 1 || sent[0].SubjectUserID != reader.ref || sent[0].Permission != GrantReview {
		t.Errorf("notices %+v", sent)
	}
}

// TestReviewGrantPathNeverNotifies (section 2.6, K31n): on the review path
// (a caller with review-grant authority who does not administer the
// artifact) notify makes the review grant as today and answers
// not_permitted without asking the notifier; a caller without that
// authority sending notify gets the uniform 403.
func TestReviewGrantPathNeverNotifies(t *testing.T) {
	f, id := newReviewGrantFixture(t)
	calls := &[]string{}
	n := &recNotifier{log: calls}
	svc := NewService(&notifyHost{fakeHost: f.host, recNotifier: n})
	svc.SetStore(f.store)
	svc.SetBlobStorage(f.blobs, "hub-1")
	f.svc = svc
	body := `{"subjectKind":"principal","subjectRef":"` + reviewerID + `","permission":"review","notify":true}`
	rec := f.postGrantBody(delegator, id, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("review path with notify: %d %s", rec.Code, rec.Body.String())
	}
	resp := decodeInto[GrantResponse](t, rec)
	if resp.Notified || resp.NotifySkipped != NotifySkippedNotPermitted || resp.Grant.Permission != GrantReview {
		t.Errorf("response %+v, want the review grant and not_permitted", resp)
	}
	if len(*calls) != 0 {
		t.Errorf("the review path asked the notifier: %q", *calls)
	}
	if v, ok := lastNotified(t, f.store, id, reviewerID); ok {
		t.Errorf("event notified = %v, want absent", v)
	}
	// Without notify the response is as before.
	plain := f.postGrantBody(delegator, id, reviewBody(SubjectPrincipal, reviewerID, GrantReview))
	if strings.Contains(plain.Body.String(), "notif") {
		t.Errorf("no-notify review response mentions notify: %s", plain.Body.String())
	}

	// A caller without review-grant authority: the uniform 403, as for the
	// same body without notify.
	want := f.postGrantBody(member, id, reviewBody(SubjectPrincipal, reviewerID, GrantReview))
	got := f.postGrantBody(member, id, body)
	if got.Code != http.StatusForbidden || got.Code != want.Code || got.Body.String() != want.Body.String() {
		t.Errorf("non-authorised caller with notify: %d %s, want %d %s", got.Code, got.Body.String(), want.Code, want.Body.String())
	}
	if len(*calls) != 0 {
		t.Errorf("the notifier was asked: %q", *calls)
	}
}

// TestGrantNotifyValidatedAfterAuthority (K17): notify is validated only
// after the caller's administration is decided: a reader who does not
// administer the artifact sending a notify body for an agent subject gets
// the uniform 403, exactly as without notify; the owner gets 400.
func TestGrantNotifyValidatedAfterAuthority(t *testing.T) {
	f := newNotifyFixture(t)
	reader := principal{PrincipalKindUser, "user-reader", ""}
	f.host.allow(reader, "project-1", PermissionRead)
	agentBody := GrantRequest{SubjectKind: SubjectPrincipal, SubjectRef: PrincipalRef(agentB.kind, agentB.ref), Permission: GrantRead, Notify: true}
	plainBody := agentBody
	plainBody.Notify = false

	want := f.postJSON(&reader, grantsPath(f.id), plainBody)
	got := f.postJSON(&reader, grantsPath(f.id), agentBody)
	if got.Code != http.StatusForbidden || got.Body.String() != want.Body.String() {
		t.Errorf("reader: %d %s, want the uniform 403 %s", got.Code, got.Body.String(), want.Body.String())
	}
	scopeBody := GrantRequest{SubjectKind: SubjectScope, SubjectRef: "project-1", Permission: GrantRead, Notify: true}
	if rec := f.postJSON(&reader, grantsPath(f.id), scopeBody); rec.Code != http.StatusForbidden {
		t.Errorf("reader, scope subject: %d, want 403", rec.Code)
	}
	for _, body := range []GrantRequest{agentBody, scopeBody} {
		rec := f.postJSON(&userU, grantsPath(f.id), body)
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != "bad_request" || !strings.Contains(rec.Body.String(), "notify is for a user") {
			t.Errorf("owner, %s subject: %d %s, want 400 notify is for a user", body.SubjectKind, rec.Code, rec.Body.String())
		}
	}
	if calls := f.take(); len(calls) != 0 {
		t.Errorf("calls %q, want none", calls)
	}
}

// TestGrantResponseNoNotifyByteIdentical: a grant request without notify
// answers exactly the body it answered before Ask for review (the new keys
// are omitempty), and the store is asked with notify=false through
// PutGrantNotify.
func TestGrantResponseNoNotifyByteIdentical(t *testing.T) {
	f := newNotifyFixture(t)
	rec := f.postJSON(&userU, grantsPath(f.id), GrantRequest{SubjectKind: SubjectPrincipal, SubjectRef: notifySubjectID, Permission: GrantRead})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d", rec.Code)
	}
	var before struct {
		Grant GrantInfo `json:"grant"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	golden, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != string(golden) {
		t.Errorf("body %s, want %s", got, golden)
	}
	if calls := f.take(); strs(calls) != "put notify=false" {
		t.Errorf("calls %q, want one PutGrantNotify with notify=false", calls)
	}
	if v, ok := lastNotified(t, f.store, f.id, notifySubjectID); ok {
		t.Errorf("event notified = %v", v)
	}
}

// TestPutGrantWrapperIdentical (spec amendment A1, C1 and C2): PutGrant is
// PutGrantNotify with notify=false: the same GrantPut and the same event
// detail, with no notified key, for create, change and no-op, on every
// dialect; and no non-test code calls PutGrant (the grant route calls
// PutGrantNotify on every path).
func TestPutGrantWrapperIdentical(t *testing.T) {
	forEachBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a1, _, _, _ := seedArtifact(t, st, "")
		a2, _, _, _ := seedArtifact(t, st, "")
		viaPut := func(id, perm string) GrantPut {
			put, err := st.PutGrant(ctx, &Grant{ID: uuid.NewString(), ArtifactID: id, SubjectKind: SubjectPrincipal,
				SubjectRef: "user:w", Permission: perm, CreatedAt: time.Now()}, 10, true, testActor)
			if err != nil {
				t.Fatal(err)
			}
			return put
		}
		viaNotify := func(id, perm string) GrantPut {
			res, err := st.PutGrantNotify(ctx, &Grant{ID: uuid.NewString(), ArtifactID: id, SubjectKind: SubjectPrincipal,
				SubjectRef: "user:w", Permission: perm, CreatedAt: time.Now()}, 10, true, testActor, false)
			if err != nil {
				t.Fatal(err)
			}
			if res.Notified {
				t.Errorf("notify=false reported Notified")
			}
			return res.Put
		}
		for _, perm := range []string{GrantRead, GrantWrite, GrantWrite, GrantRead} {
			if p1, p2 := viaPut(a1.ID, perm), viaNotify(a2.ID, perm); p1 != p2 {
				t.Errorf("%s: PutGrant %v, PutGrantNotify(false) %v", perm, p1, p2)
			}
		}
		e1, e2 := grantEvents(t, st, a1.ID, "user:w"), grantEvents(t, st, a2.ID, "user:w")
		if len(e1) != 3 || len(e1) != len(e2) {
			t.Fatalf("events %d and %d, want 3 each (create, change, change)", len(e1), len(e2))
		}
		for i := range e1 {
			d1, d2 := e1[i].Detail, e2[i].Detail
			delete(d1, "grantId")
			delete(d2, "grantId")
			if _, ok := d2["notified"]; ok {
				t.Errorf("PutGrantNotify(false) event %d carries notified", i)
			}
			j1, _ := json.Marshal(d1)
			j2, _ := json.Marshal(d2)
			if e1[i].Action != e2[i].Action || string(j1) != string(j2) {
				t.Errorf("event %d: %s %s vs %s %s", i, e1[i].Action, j1, e2[i].Action, j2)
			}
		}
	})

	// C2: no non-test file of the package calls PutGrant.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "PutGrant" {
				t.Errorf("%s: non-test code calls PutGrant; the grant route must call PutGrantNotify", fset.Position(call.Pos()))
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("no source files scanned")
	}
}

// TestGrantNotifyNoticeFailure (O5): when NotifyGrant fails after the
// commit, the grant stands, the response says notified false and
// notifySkipped unavailable, the append-only event keeps notified true,
// and the decision log records notify_failed.
func TestGrantNotifyNoticeFailure(t *testing.T) {
	f := newNotifyFixture(t)
	logs := captureLog(f.fixture, slog.LevelInfo)
	f.n.notifyErr = errors.New("insert failed")
	rec, resp := f.notifyGrant(userU, f.id, SubjectPrincipal, notifySubjectID, GrantWrite)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if resp.Notified || resp.NotifySkipped != NotifySkippedUnavailable {
		t.Errorf("notified=%v skipped=%q, want false unavailable", resp.Notified, resp.NotifySkipped)
	}
	if g := f.grantOf(f.id, notifySubjectID); g == nil || g.Permission != GrantWrite {
		t.Errorf("grant rolled back: %+v", g)
	}
	if v, ok := lastNotified(t, f.store, f.id, notifySubjectID); !ok || v != true {
		t.Errorf("event notified = %v %v, want true (append-only)", v, ok)
	}
	entries, _ := logs.take(t)
	var failed, grant bool
	for _, e := range entries {
		if e[logKeyReason] == logReasonNotifyFailed {
			failed = true
		}
		if e[logKeyAction] == logActionGrantCreated {
			grant = true
			if e[logKeyNotified] != false {
				t.Errorf("grant entry notified = %v, want false", e[logKeyNotified])
			}
		}
	}
	if !failed || !grant {
		t.Errorf("decision log: notify_failed %v, grant entry %v: %v", failed, grant, entries)
	}
}
