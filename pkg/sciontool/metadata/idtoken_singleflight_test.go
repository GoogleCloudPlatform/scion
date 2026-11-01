package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// idFlightHarness drives handleIdentityToken with a hub stub that holds every
// fetch for an audience until the test releases that audience, so leaders
// and followers can be lined up without sleeps.
type idFlightHarness struct {
	srv      *Server
	calls    atomic.Int64
	entered  chan string // one value per hub fetch, naming its audience
	joined   chan string // one value per follower parked on a leader
	mu       sync.Mutex
	releases map[string]chan struct{}
	panics   map[string]int // fetches still to panic, per audience
}

type idFlightResult struct {
	code     int
	body     string
	panicked any
}

func newIDFlightHarness(t *testing.T) *idFlightHarness {
	t.Helper()
	h := &idFlightHarness{
		entered:  make(chan string, 8),
		joined:   make(chan string, 8),
		releases: map[string]chan struct{}{},
		panics:   map[string]int{},
	}
	h.srv = New(Config{
		Mode:      "assign",
		SAEmail:   "agent@project.iam.gserviceaccount.com",
		TokenFunc: func() string { return "credential-1" },
		FetchGCPIdentityToken: func(ctx context.Context, audience string) (string, error) {
			h.calls.Add(1)
			if h.takePanic(audience) {
				panic("hub fetch stub panics for " + audience)
			}
			release := h.release(audience)
			h.entered <- audience
			<-release
			return "id-token-" + audience, nil
		},
	})
	h.srv.afterFollowerJoin = func(audience string) { h.joined <- audience }
	return h
}

// release returns the channel that holds audience's fetches; closing it
// (releaseAudience) lets them all return.
func (h *idFlightHarness) release(audience string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.releases[audience]
	if !ok {
		ch = make(chan struct{})
		h.releases[audience] = ch
	}
	return ch
}

func (h *idFlightHarness) releaseAudience(audience string) { close(h.release(audience)) }

// panicNext makes the next fetch for audience panic before it blocks.
func (h *idFlightHarness) panicNext(audience string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.panics[audience]++
}

func (h *idFlightHarness) takePanic(audience string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.panics[audience] == 0 {
		return false
	}
	h.panics[audience]--
	return true
}

// start runs one identity-token request in its own goroutine. The handler is
// called directly, so a panic is reported in the result instead of being
// recovered by net/http.
func (h *idFlightHarness) start(ctx context.Context, audience string) <-chan idFlightResult {
	out := make(chan idFlightResult, 1)
	go func() {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/identity?audience="+audience, nil).WithContext(ctx)
		var res idFlightResult
		func() {
			defer func() { res.panicked = recover() }()
			h.srv.handleIdentityToken(w, r)
		}()
		res.code, res.body = w.Code, w.Body.String()
		out <- res
	}()
	return out
}

const idFlightWait = 5 * time.Second

func waitEntered(t *testing.T, h *idFlightHarness, want string) {
	t.Helper()
	select {
	case got := <-h.entered:
		if got != want {
			t.Fatalf("hub fetch for %q, want %q", got, want)
		}
	case <-time.After(idFlightWait):
		t.Fatalf("no hub fetch for %q", want)
	}
}

func waitResult(t *testing.T, name string, ch <-chan idFlightResult) idFlightResult {
	t.Helper()
	select {
	case res := <-ch:
		if res.panicked != nil {
			t.Fatalf("%s panicked: %v", name, res.panicked)
		}
		return res
	case <-time.After(idFlightWait):
		t.Fatalf("%s did not return", name)
		return idFlightResult{}
	}
}

func wantToken(t *testing.T, name string, res idFlightResult, audience string) {
	t.Helper()
	if res.code != http.StatusOK || res.body != "id-token-"+audience {
		t.Fatalf("%s: %d %q, want 200 %q", name, res.code, res.body, "id-token-"+audience)
	}
}

// Two leaders for different audiences fetch at the same time; each finishes
// on its own, with no double close and its own token.
func TestIDTokenSingleflight_DifferentAudiencesNoDoubleClose(t *testing.T) {
	h := newIDFlightHarness(t)
	ctx := context.Background()
	a := h.start(ctx, "aud-a")
	waitEntered(t, h, "aud-a")
	b := h.start(ctx, "aud-b")
	waitEntered(t, h, "aud-b")

	h.releaseAudience("aud-a")
	wantToken(t, "aud-a leader", waitResult(t, "aud-a leader", a), "aud-a")
	h.releaseAudience("aud-b")
	wantToken(t, "aud-b leader", waitResult(t, "aud-b leader", b), "aud-b")

	if got := h.calls.Load(); got != 2 {
		t.Fatalf("hub calls: %d, want 2", got)
	}
}

// While two audiences are in flight, a second request for the first audience
// waits on that audience's leader (no second hub call) and gets its token
// when that leader finishes, whatever the other audience is doing.
func TestIDTokenSingleflight_FollowerJoinsItsOwnAudience(t *testing.T) {
	h := newIDFlightHarness(t)
	ctx := context.Background()
	a := h.start(ctx, "aud-a")
	waitEntered(t, h, "aud-a")
	b := h.start(ctx, "aud-b")
	waitEntered(t, h, "aud-b")

	a2 := h.start(ctx, "aud-a")
	select {
	case got := <-h.joined:
		if got != "aud-a" {
			t.Fatalf("follower joined %q, want aud-a", got)
		}
	case got := <-h.entered:
		h.releaseAudience("aud-a")
		h.releaseAudience("aud-b")
		t.Fatalf("the second aud-a request became a leader (hub fetch for %q) instead of following", got)
	case <-time.After(idFlightWait):
		t.Fatal("the second aud-a request neither followed nor fetched")
	}

	h.releaseAudience("aud-a")
	wantToken(t, "aud-a leader", waitResult(t, "aud-a leader", a), "aud-a")
	wantToken(t, "aud-a follower", waitResult(t, "aud-a follower", a2), "aud-a")
	h.releaseAudience("aud-b")
	wantToken(t, "aud-b leader", waitResult(t, "aud-b leader", b), "aud-b")

	if got := h.calls.Load(); got != 2 {
		t.Fatalf("hub calls: %d, want 2", got)
	}
}

// A follower whose request context ends stops waiting at once; the leader
// still finishes and caches the token for later requests.
func TestIDTokenSingleflight_FollowerContextCancel(t *testing.T) {
	h := newIDFlightHarness(t)
	a := h.start(context.Background(), "aud-a")
	waitEntered(t, h, "aud-a")

	ctx, cancel := context.WithCancel(context.Background())
	a2 := h.start(ctx, "aud-a")
	select {
	case <-h.joined:
	case <-time.After(idFlightWait):
		h.releaseAudience("aud-a")
		t.Fatal("the second aud-a request did not follow the leader")
	}
	cancel()
	select {
	case res := <-a2:
		if res.panicked != nil {
			t.Fatalf("follower panicked: %v", res.panicked)
		}
	case <-time.After(idFlightWait):
		h.releaseAudience("aud-a")
		t.Fatal("the follower kept waiting after its request context ended")
	}

	h.releaseAudience("aud-a")
	wantToken(t, "aud-a leader", waitResult(t, "aud-a leader", a), "aud-a")
	wantToken(t, "cached aud-a", waitResult(t, "cached aud-a", h.start(context.Background(), "aud-a")), "aud-a")
	if got := h.calls.Load(); got != 1 {
		t.Fatalf("hub calls: %d, want 1", got)
	}
}

// Same-audience requests still collapse to one hub call.
func TestIDTokenSingleflight_SameAudienceOneCall(t *testing.T) {
	h := newIDFlightHarness(t)
	ctx := context.Background()
	a := h.start(ctx, "aud-a")
	waitEntered(t, h, "aud-a")
	a2 := h.start(ctx, "aud-a")
	select {
	case <-h.joined:
	case <-time.After(idFlightWait):
		h.releaseAudience("aud-a")
		t.Fatal("the second aud-a request did not follow the leader")
	}
	h.releaseAudience("aud-a")
	wantToken(t, "leader", waitResult(t, "leader", a), "aud-a")
	wantToken(t, "follower", waitResult(t, "follower", a2), "aud-a")
	if got := h.calls.Load(); got != 1 {
		t.Fatalf("hub calls: %d, want 1", got)
	}
}

// A leader whose fetch panics still removes its in-flight entry, so the next
// request for that audience leads a new fetch instead of waiting on a leader
// that will never finish.
func TestIDTokenSingleflight_LeaderPanicLeavesNoEntry(t *testing.T) {
	h := newIDFlightHarness(t)
	ctx := context.Background()
	h.panicNext("aud-a")
	select {
	case res := <-h.start(ctx, "aud-a"):
		if res.panicked == nil {
			t.Fatal("the first aud-a request did not panic (stub)")
		}
	case <-time.After(idFlightWait):
		t.Fatal("the panicking aud-a request did not return")
	}

	a := h.start(ctx, "aud-a")
	select {
	case got := <-h.entered:
		if got != "aud-a" {
			t.Fatalf("hub fetch for %q, want aud-a", got)
		}
	case got := <-h.joined:
		t.Fatalf("the next %s request waited on the dead leader instead of leading", got)
	case <-time.After(idFlightWait):
		t.Fatal("the next aud-a request neither led nor followed")
	}
	h.releaseAudience("aud-a")
	wantToken(t, "aud-a after the panic", waitResult(t, "aud-a after the panic", a), "aud-a")
	if got := h.calls.Load(); got != 2 {
		t.Fatalf("hub calls: %d, want 2", got)
	}
}
