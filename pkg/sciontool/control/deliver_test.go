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

package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
)

// fakeTmux is a fake Runner: it records every argv and load-buffer stdin,
// fails the verbs in fail, and answers the readiness probe with probeOut.
type fakeTmux struct {
	mu       sync.Mutex
	calls    [][]string
	stdin    []string
	fail     map[string]error // by tmux verb, or "sh" for the probe
	failNth  map[string]int   // fail only the n-th call of a verb (1-based)
	seen     map[string]int
	probeOut string
	block    chan struct{} // when set, load-buffer waits on it
	entered  chan struct{} // closed on the first load-buffer
	once     sync.Once
	onVerb   func(verb string) // called before each command, outside mu
}

func (f *fakeTmux) Run(ctx context.Context, argv []string, stdin io.Reader) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(argv))
	verb := argv[0]
	if argv[0] == "tmux" && len(argv) > 1 {
		verb = argv[1]
	}
	if f.seen == nil {
		f.seen = map[string]int{}
	}
	f.seen[verb]++
	n := f.seen[verb]
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		f.stdin = append(f.stdin, string(b))
	}
	err := f.fail[verb]
	if nth, ok := f.failNth[verb]; ok && nth != n {
		err = nil
	}
	block, entered, onVerb := f.block, f.entered, f.onVerb
	f.mu.Unlock()
	if onVerb != nil {
		onVerb(verb)
	}
	if verb == "load-buffer" && entered != nil {
		f.once.Do(func() { close(entered) })
	}
	if verb == "load-buffer" && block != nil {
		<-block
	}
	if verb == "sh" {
		return f.probeOut, err
	}
	return "", err
}

func (f *fakeTmux) verbs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c[0] == "tmux" {
			out = append(out, c[1])
		} else {
			out = append(out, c[0])
		}
	}
	return out
}

func strp(s string) *string { return &s }

func baseReq(now time.Time) agentcontrol.DeliverRequest {
	return agentcontrol.DeliverRequest{
		V:              1,
		DeliveryID:     "d-1",
		BrokerID:       "broker-1",
		MessageIDs:     []string{"m-1"},
		Op:             agentcontrol.OpPaste,
		Text:           strp("hello"),
		DeadlineMS:     92000,
		NotAfterUnixMS: now.Add(92 * time.Second).UnixMilli(),
		SubmitKeys:     []string{"Enter"},
		ExtraSubmits:   &agentcontrol.ExtraSubmits{Count: 2, DelayMS: 1},
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	c.Advance(d)
	if dl, ok := ctx.Deadline(); ok && !c.Now().Before(dl) {
		return false
	}
	return true
}

func newDeliverHandler(f *fakeTmux, clk *fakeClock) *Handler {
	o := &DeliverOptions{Runner: f}
	if clk != nil {
		o.Now, o.Sleep = clk.Now, clk.Sleep
	}
	return New(Options{KickTokenRefresh: func() {}, Deliver: o})
}

func post(t *testing.T, h http.Handler, body any) (int, agentcontrol.DeliverResponse) {
	t.Helper()
	var raw []byte
	switch b := body.(type) {
	case []byte:
		raw = b
	default:
		var err error
		if raw, err = json.Marshal(b); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, Prefix+RouteDeliverMessage, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp agentcontrol.DeliverResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("answer %d is not JSON: %q", rec.Code, rec.Body.String())
	}
	return rec.Code, resp
}

func TestDeliverRouteRegisteredOnlyWithOptions(t *testing.T) {
	if slices.Contains(New(Options{KickTokenRefresh: func() {}}).Routes(), RouteDeliverMessage) {
		t.Fatal("deliver-message advertised without Deliver options")
	}
	h := newDeliverHandler(&fakeTmux{}, nil)
	if !slices.Contains(h.Routes(), RouteDeliverMessage) {
		t.Fatal("deliver-message not advertised with Deliver options")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+RouteDeliverMessage, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", rec.Code)
	}
}

func TestDeliverEnabled(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == EnvHubExperiments {
				return v
			}
			return ""
		}
	}
	for _, tc := range []struct {
		env  string
		tmux bool
		want bool
	}{
		{"hub.conduit,hub.conduit_deliver", true, true},
		{" hub.conduit_deliver ", true, true},
		{"hub.conduit_deliver", false, false},
		{"hub.conduit", true, false},
		{"hub.conduit-deliver", true, false},
		{"", true, false},
	} {
		if got := DeliverEnabled(env(tc.env), tc.tmux); got != tc.want {
			t.Errorf("DeliverEnabled(%q, %t) = %t, want %t", tc.env, tc.tmux, got, tc.want)
		}
	}
}

// TestDeliverSchemaRejects: every malformed or out-of-bounds field is a 400
// naming the field, and nothing runs (no generic exec: fixed schema, key
// allowlist, no caller argv).
func TestDeliverSchemaRejects(t *testing.T) {
	now := time.Now()
	cases := map[string]struct {
		mod   func(r *agentcontrol.DeliverRequest)
		field string
	}{
		"v2":                     {func(r *agentcontrol.DeliverRequest) { r.V = 2 }, agentcontrol.FieldV},
		"delivery id shape":      {func(r *agentcontrol.DeliverRequest) { r.DeliveryID = "a b" }, agentcontrol.FieldDeliveryID},
		"delivery id long":       {func(r *agentcontrol.DeliverRequest) { r.DeliveryID = strings.Repeat("a", 65) }, agentcontrol.FieldDeliveryID},
		"no broker id":           {func(r *agentcontrol.DeliverRequest) { r.BrokerID = "" }, agentcontrol.FieldBrokerID},
		"no message ids":         {func(r *agentcontrol.DeliverRequest) { r.MessageIDs = nil }, agentcontrol.FieldMessageIDs},
		"bad message id":         {func(r *agentcontrol.DeliverRequest) { r.MessageIDs = []string{"../x"} }, agentcontrol.FieldMessageIDs},
		"duplicate message id":   {func(r *agentcontrol.DeliverRequest) { r.MessageIDs = []string{"a", "a"} }, agentcontrol.FieldMessageIDs},
		"unknown op":             {func(r *agentcontrol.DeliverRequest) { r.Op = "exec" }, agentcontrol.FieldOp},
		"raw op":                 {func(r *agentcontrol.DeliverRequest) { r.Op = "raw" }, agentcontrol.FieldOp},
		"paste without text":     {func(r *agentcontrol.DeliverRequest) { r.Text = nil }, agentcontrol.FieldText},
		"paste empty text":       {func(r *agentcontrol.DeliverRequest) { r.Text = strp("") }, agentcontrol.FieldText},
		"submit_only with text":  {func(r *agentcontrol.DeliverRequest) { r.Op = agentcontrol.OpSubmitOnly; r.ExtraSubmits = nil }, agentcontrol.FieldText},
		"interrupt without keys": {func(r *agentcontrol.DeliverRequest) { r.Op = agentcontrol.OpInterruptPaste }, agentcontrol.FieldInterruptKeys},
		"stray interrupt keys":   {func(r *agentcontrol.DeliverRequest) { r.InterruptKeys = []string{"C-c"} }, agentcontrol.FieldInterruptKeys},
		"five interrupt keys": {func(r *agentcontrol.DeliverRequest) {
			r.Op = agentcontrol.OpInterruptPaste
			r.InterruptKeys = []string{"C-c", "C-c", "C-c", "C-c", "C-c"}
		}, agentcontrol.FieldInterruptKeys},
		"interrupt key -l": {func(r *agentcontrol.DeliverRequest) {
			r.Op = agentcontrol.OpInterruptPaste
			r.InterruptKeys = []string{"-l"}
		}, agentcontrol.FieldInterruptKeys},
		"interrupt key C-d": {func(r *agentcontrol.DeliverRequest) {
			r.Op = agentcontrol.OpInterruptPaste
			r.InterruptKeys = []string{"C-d"}
		}, agentcontrol.FieldInterruptKeys},
		"no deadline":        {func(r *agentcontrol.DeliverRequest) { r.DeadlineMS = 0 }, agentcontrol.FieldDeadlineMS},
		"deadline 92.001s":   {func(r *agentcontrol.DeliverRequest) { r.DeadlineMS = 92001 }, agentcontrol.FieldDeadlineMS},
		"no not_after":       {func(r *agentcontrol.DeliverRequest) { r.NotAfterUnixMS = 0 }, agentcontrol.FieldNotAfter},
		"no submit keys":     {func(r *agentcontrol.DeliverRequest) { r.SubmitKeys = nil }, agentcontrol.FieldSubmitKeys},
		"literal submit key": {func(r *agentcontrol.DeliverRequest) { r.SubmitKeys = []string{"rm -rf /"} }, agentcontrol.FieldSubmitKeys},
		"five submit keys": {func(r *agentcontrol.DeliverRequest) {
			r.SubmitKeys = []string{"Enter", "Enter", "Enter", "Enter", "Enter"}
		}, agentcontrol.FieldSubmitKeys},
		"4 extra submits":       {func(r *agentcontrol.DeliverRequest) { r.ExtraSubmits.Count = 4 }, agentcontrol.FieldExtraSubmits},
		"3s extra delay":        {func(r *agentcontrol.DeliverRequest) { r.ExtraSubmits.DelayMS = 3000 }, agentcontrol.FieldExtraDelay},
		"extras on submit_only": {func(r *agentcontrol.DeliverRequest) { r.Op = agentcontrol.OpSubmitOnly; r.Text = nil }, agentcontrol.FieldExtraSubmits},
		"ready on interrupt": {func(r *agentcontrol.DeliverRequest) {
			r.Op = agentcontrol.OpInterruptPaste
			r.InterruptKeys = []string{"C-c"}
			r.Ready = &agentcontrol.ReadySpec{Strategy: "hook"}
		}, agentcontrol.FieldReady},
		"ready strategy settle": {func(r *agentcontrol.DeliverRequest) { r.Ready = &agentcontrol.ReadySpec{Strategy: "settle"} }, agentcontrol.FieldReadyStrategy},
		"ready settle 11s": {func(r *agentcontrol.DeliverRequest) {
			r.Ready = &agentcontrol.ReadySpec{Strategy: "delay", SettleMS: 11000}
		}, agentcontrol.FieldReadySettle},
		"ready timeout 91s": {func(r *agentcontrol.DeliverRequest) {
			r.Ready = &agentcontrol.ReadySpec{Strategy: "hook", TimeoutMS: 91000}
		}, agentcontrol.FieldReadyTimeout},
		"text only control chars": {func(r *agentcontrol.DeliverRequest) { r.Text = strp("\x1b\x07\r\u009b") }, agentcontrol.FieldText},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeTmux{}
			r := baseReq(now)
			tc.mod(&r)
			code, resp := post(t, newDeliverHandler(f, nil), r)
			if code != http.StatusBadRequest || resp.Outcome != agentcontrol.OutcomeInvalid || resp.Field != tc.field {
				t.Fatalf("= %d %+v, want 400 invalid %s", code, resp, tc.field)
			}
			for _, v := range f.verbs() {
				if v != "has-session" {
					t.Fatalf("ran %v for a refused request", f.verbs())
				}
			}
		})
	}
	// A passing control.
	f := &fakeTmux{}
	if code, resp := post(t, newDeliverHandler(f, nil), baseReq(now)); code != http.StatusOK || resp.Outcome != agentcontrol.OutcomeDelivered {
		t.Fatalf("control = %d %+v", code, resp)
	}
}

func manyIDs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("m-%d", i)
	}
	return out
}

// TestDeliverBodyRejects: unknown fields (an argv, a route name, a tmux
// target), trailing data and a non-JSON content type are refused.
func TestDeliverBodyRejects(t *testing.T) {
	now := time.Now()
	good, _ := json.Marshal(baseReq(now))
	withField := func(k string, v any) []byte {
		var m map[string]any
		_ = json.Unmarshal(good, &m)
		m[k] = v
		b, _ := json.Marshal(m)
		return b
	}
	for name, body := range map[string][]byte{
		"argv":     withField("argv", []string{"sh", "-c", "id"}),
		"route":    withField("route", "exec-start"),
		"target":   withField("target", "other:1"),
		"nonce":    withField("ready_nonce", "x"),
		"trailing": append(append([]byte{}, good...), []byte(` {}`)...),
		"not json": []byte("v=1"),
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeTmux{}
			code, resp := post(t, newDeliverHandler(f, nil), body)
			if code != http.StatusBadRequest || resp.Field != agentcontrol.FieldBody {
				t.Fatalf("= %d %+v, want 400 body", code, resp)
			}
			if len(f.verbs()) != 0 {
				t.Fatalf("ran %v", f.verbs())
			}
		})
	}
	req := httptest.NewRequest(http.MethodPost, Prefix+RouteDeliverMessage, strings.NewReader(string(good)))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	newDeliverHandler(&fakeTmux{}, nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("text/plain = %d, want 400", rec.Code)
	}
}

// TestDeliverPasteSteps: the argv is built from the fixed schema: target
// constant, buffer named from delivery_id, text only on load-buffer's stdin
// (sanitised), never in argv.
func TestDeliverPasteSteps(t *testing.T) {
	const sentinel = "SENTINEL-TEXT-7353"
	f := &fakeTmux{}
	r := baseReq(time.Now())
	r.Text = strp(sentinel + "\x1b[201~tail\u009b")
	code, resp := post(t, newDeliverHandler(f, nil), r)
	if code != http.StatusOK || resp.Outcome != agentcontrol.OutcomeDelivered || resp.Replayed {
		t.Fatalf("= %d %+v", code, resp)
	}
	want := [][]string{
		{"tmux", "has-session", "-t", "scion:0"},
		{"tmux", "load-buffer", "-b", "scion-msg-d-1-1", "-"},
		{"tmux", "paste-buffer", "-t", "scion:0", "-p", "-d", "-b", "scion-msg-d-1-1"},
		{"tmux", "send-keys", "-t", "scion:0", "Enter"},
		{"tmux", "send-keys", "-t", "scion:0", "Enter"},
		{"tmux", "send-keys", "-t", "scion:0", "Enter"},
	}
	if !slices.EqualFunc(f.calls, want, slices.Equal[[]string]) {
		t.Fatalf("calls =\n%v\nwant\n%v", f.calls, want)
	}
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), sentinel) {
			t.Fatalf("text in argv: %v", c)
		}
	}
	if len(f.stdin) != 1 || f.stdin[0] != sentinel+"[201~tail" {
		t.Fatalf("stdin = %q, want the sanitised text", f.stdin)
	}
}

// TestDeliverOps: each op runs exactly its step list: interrupt ops send
// the keys first and never probe readiness; submit ops send the submit keys
// once with no extras.
func TestDeliverOps(t *testing.T) {
	now := time.Now()
	cases := map[string]struct {
		mod  func(r *agentcontrol.DeliverRequest)
		want []string
	}{
		"interrupt_paste": {func(r *agentcontrol.DeliverRequest) {
			r.Op = agentcontrol.OpInterruptPaste
			r.InterruptKeys = []string{"Escape", "C-c"}
			r.ExtraSubmits = &agentcontrol.ExtraSubmits{Count: 1}
		}, []string{"has-session", "send-keys Escape", "send-keys C-c", "load-buffer", "paste-buffer", "send-keys Enter", "send-keys Enter"}},
		"submit_only": {func(r *agentcontrol.DeliverRequest) {
			r.Op = agentcontrol.OpSubmitOnly
			r.Text, r.ExtraSubmits = nil, nil
		}, []string{"has-session", "send-keys Enter"}},
		"interrupt_submit": {func(r *agentcontrol.DeliverRequest) {
			r.Op = agentcontrol.OpInterruptSubmit
			r.Text, r.ExtraSubmits = nil, nil
			r.InterruptKeys = []string{"C-c"}
		}, []string{"has-session", "send-keys C-c", "send-keys Enter"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeTmux{}
			r := baseReq(now)
			tc.mod(&r)
			if code, resp := post(t, newDeliverHandler(f, nil), r); code != http.StatusOK || resp.Outcome != agentcontrol.OutcomeDelivered {
				t.Fatalf("= %d %+v", code, resp)
			}
			var got []string
			for _, c := range f.calls {
				s := c[1]
				if s == "send-keys" {
					s += " " + c[4]
				}
				got = append(got, s)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("steps = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDeliverIdempotency: a finished key replays its stored outcome with no
// tmux call; a failure before any visible step is not stored, so the same
// key runs again.
func TestDeliverIdempotency(t *testing.T) {
	f := &fakeTmux{}
	h := newDeliverHandler(f, nil)
	r := baseReq(time.Now())
	if code, _ := post(t, h, r); code != http.StatusOK {
		t.Fatal(code)
	}
	n := len(f.verbs())
	code, resp := post(t, h, r)
	if code != http.StatusOK || resp.Outcome != agentcontrol.OutcomeDelivered || !resp.Replayed {
		t.Fatalf("replay = %d %+v", code, resp)
	}
	if len(f.verbs()) != n {
		t.Fatalf("replay ran tmux: %v", f.verbs()[n:])
	}
	// The key is scoped by broker: another broker's same delivery id runs.
	r2 := r
	r2.BrokerID = "broker-2"
	if _, resp := post(t, h, r2); resp.Replayed {
		t.Fatal("another broker's key replayed")
	}

	// load-buffer failure: 422, not stored; the retry runs.
	f2 := &fakeTmux{fail: map[string]error{"load-buffer": errors.New("boom")}}
	h2 := newDeliverHandler(f2, nil)
	code, resp = post(t, h2, r)
	if code != http.StatusUnprocessableEntity || resp.Stage != agentcontrol.StageLoadBuffer {
		t.Fatalf("load-buffer failure = %d %+v", code, resp)
	}
	f2.mu.Lock()
	f2.fail = nil
	f2.mu.Unlock()
	if code, resp = post(t, h2, r); code != http.StatusOK || resp.Replayed {
		t.Fatalf("retry after 422 = %d %+v", code, resp)
	}

	// Missing target: 422 target, nothing else runs.
	f3 := &fakeTmux{fail: map[string]error{"has-session": errors.New("no server")}}
	code, resp = post(t, newDeliverHandler(f3, nil), r)
	if code != http.StatusUnprocessableEntity || resp.Stage != agentcontrol.StageTarget || len(f3.verbs()) != 1 {
		t.Fatalf("missing target = %d %+v %v", code, resp, f3.verbs())
	}
}

// TestDeliverPartial: a failure after a visible step is partial (stored,
// never re-run), naming the stage.
func TestDeliverPartial(t *testing.T) {
	now := time.Now()
	cases := map[string]struct {
		f     *fakeTmux
		mod   func(r *agentcontrol.DeliverRequest)
		stage string
	}{
		"submit": {&fakeTmux{fail: map[string]error{"send-keys": errors.New("x")}, failNth: map[string]int{"send-keys": 1}}, nil, agentcontrol.StageSubmit},
		"extra":  {&fakeTmux{fail: map[string]error{"send-keys": errors.New("x")}, failNth: map[string]int{"send-keys": 2}}, nil, agentcontrol.StageExtraSubmit},
		"paste":  {&fakeTmux{fail: map[string]error{"paste-buffer": errors.New("x")}}, nil, agentcontrol.StagePaste},
		"interrupt": {&fakeTmux{fail: map[string]error{"send-keys": errors.New("x")}, failNth: map[string]int{"send-keys": 1}}, func(r *agentcontrol.DeliverRequest) {
			r.Op = agentcontrol.OpInterruptPaste
			r.InterruptKeys = []string{"C-c"}
		}, agentcontrol.StageInterrupt},
		"load after interrupt": {&fakeTmux{fail: map[string]error{"load-buffer": errors.New("x")}}, func(r *agentcontrol.DeliverRequest) {
			r.Op = agentcontrol.OpInterruptPaste
			r.InterruptKeys = []string{"C-c"}
		}, agentcontrol.StageLoadBuffer},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := baseReq(now)
			if tc.mod != nil {
				tc.mod(&r)
			}
			h := newDeliverHandler(tc.f, nil)
			code, resp := post(t, h, r)
			if code != http.StatusOK || resp.Outcome != agentcontrol.OutcomePartial || resp.Stage != tc.stage {
				t.Fatalf("= %d %+v, want partial %s", code, resp, tc.stage)
			}
			n := len(tc.f.verbs())
			if _, resp := post(t, h, r); !resp.Replayed || resp.Outcome != agentcontrol.OutcomePartial {
				t.Fatalf("replay = %+v", resp)
			}
			if len(tc.f.verbs()) != n {
				t.Fatal("partial replay ran tmux again")
			}
		})
	}
}

// TestDeliverBusyAndInProgress: while one flush runs, the same key gets 409
// in_progress and another key 409 busy; neither runs anything.
func TestDeliverBusyAndInProgress(t *testing.T) {
	f := &fakeTmux{block: make(chan struct{}), entered: make(chan struct{})}
	h := newDeliverHandler(f, nil)
	r := baseReq(time.Now())
	done := make(chan int)
	go func() {
		code, _ := post(t, h, r)
		done <- code
	}()
	<-f.entered
	if code, resp := post(t, h, r); code != http.StatusConflict || resp.Outcome != agentcontrol.OutcomeInProgress {
		t.Fatalf("same key = %d %+v", code, resp)
	}
	other := r
	other.DeliveryID = "d-2"
	if code, resp := post(t, h, other); code != http.StatusConflict || resp.Outcome != agentcontrol.OutcomeBusy {
		t.Fatalf("other key = %d %+v", code, resp)
	}
	close(f.block)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("first = %d", code)
	}
	if code, resp := post(t, h, other); code != http.StatusOK || resp.Replayed {
		t.Fatalf("other key after = %d %+v", code, resp)
	}
}

func TestDeliverStoreBounds(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	s := newDeliverStore(3, time.Minute, clk.Now)
	for i := range 4 {
		s.put(fmt.Sprint(i), deliverResult{status: 200})
	}
	if s.len() != 3 {
		t.Fatalf("len = %d, want 3", s.len())
	}
	if _, ok := s.get("0"); ok {
		t.Fatal("LRU entry 0 not evicted")
	}
	s.get("1") // 1 is now most recent
	s.put("4", deliverResult{})
	if _, ok := s.get("2"); ok {
		t.Fatal("entry 2 should be evicted after 1 was used")
	}
	if _, ok := s.get("1"); !ok {
		t.Fatal("recently used entry 1 evicted")
	}
	clk.Advance(time.Minute + time.Second)
	if _, ok := s.get("1"); ok {
		t.Fatal("entry kept past TTL")
	}
	if deliverStoreMax != 1024 || deliverStoreTTL != 15*time.Minute {
		t.Fatal("key store bounds changed from the design (1024, 15 min)")
	}
}

func TestHardStop(t *testing.T) {
	rcpt := time.Unix(10_000, 0)
	ms := func(d time.Duration) int64 { return rcpt.Add(d).UnixMilli() }
	cases := []struct {
		name     string
		notAfter int64
		want     time.Duration
		skewed   bool
	}{
		{"not_after later than deadline", ms(100 * time.Second), 92 * time.Second, false},
		{"not_after inside tolerance", ms(70 * time.Second), 70 * time.Second, false},
		{"not_after at edge", ms(62 * time.Second), 62 * time.Second, false},
		{"not_after too early (skew)", ms(61 * time.Second), 92 * time.Second, true},
		{"not_after past (agent clock 120s ahead)", ms(-28 * time.Second), 92 * time.Second, true},
	}
	for _, tc := range cases {
		stop, skewed := hardStop(rcpt, 92000, tc.notAfter)
		if got := stop.Sub(rcpt); got != tc.want || skewed != tc.skewed {
			t.Errorf("%s: stop=+%s skewed=%t, want +%s %t", tc.name, got, skewed, tc.want, tc.skewed)
		}
	}
}

// hookNeverReady is a probe answer: hook mode, session up, no marker.
const hookNeverReady = "session=100\nnow=200\ninitrun=nonce=abcd1234 mode=hook\ninitspec=\nready=\nreadymtime=\n"

// TestDeliverReadinessPastHardStop: a readiness wait that runs past the
// hard stop answers 408 and never loads or pastes; a replay of that key
// answers 408 again without running anything.
func TestDeliverReadinessPastHardStop(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	f := &fakeTmux{probeOut: hookNeverReady}
	h := newDeliverHandler(f, clk)
	r := baseReq(clk.Now())
	r.NotAfterUnixMS = clk.Now().Add(70 * time.Second).UnixMilli() // inside tolerance: used
	r.Ready = &agentcontrol.ReadySpec{Strategy: "hook", SettleMS: 1500, TimeoutMS: 90000}
	code, resp := post(t, h, r)
	if code != http.StatusRequestTimeout || resp.Outcome != agentcontrol.OutcomeExpiredBeforePaste {
		t.Fatalf("= %d %+v, want 408", code, resp)
	}
	for _, v := range f.verbs() {
		if v == "load-buffer" || v == "paste-buffer" || v == "send-keys" {
			t.Fatalf("ran %s after the hard stop", v)
		}
	}
	n := len(f.verbs())
	if code, resp := post(t, h, r); code != http.StatusRequestTimeout || !resp.Replayed {
		t.Fatalf("replay = %d %+v", code, resp)
	}
	if len(f.verbs()) != n {
		t.Fatal("replay of an expired key ran tmux")
	}
}

// TestDeliverReadinessTimeoutThenPaste: with the receipt bound (92 s), a
// hook wait that reaches its own 90 s deadline pastes anyway (the existing
// semantics).
func TestDeliverReadinessTimeoutThenPaste(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	f := &fakeTmux{probeOut: hookNeverReady}
	r := baseReq(clk.Now())
	r.Ready = &agentcontrol.ReadySpec{Strategy: "hook", SettleMS: 1500, TimeoutMS: 90000}
	code, resp := post(t, newDeliverHandler(f, clk), r)
	if code != http.StatusOK || resp.Outcome != agentcontrol.OutcomeDelivered {
		t.Fatalf("= %d %+v", code, resp)
	}
	probes := 0
	for _, v := range f.verbs() {
		if v == "sh" {
			probes++
		}
	}
	if probes < 100 || !slices.Contains(f.verbs(), "paste-buffer") {
		t.Fatalf("%d probes, verbs end %v; want the full 90s of probes then a paste", probes, f.verbs()[len(f.verbs())-3:])
	}
}

// TestDeliverReadinessDelayReady: delay strategy, session old enough: one
// probe, no settle, then the paste.
func TestDeliverReadinessDelayReady(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	f := &fakeTmux{probeOut: "session=100\nnow=200\ninitrun=nonce=abcd1234 mode=settle\ninitspec=\nready=\nreadymtime=\n"}
	r := baseReq(clk.Now())
	r.Ready = &agentcontrol.ReadySpec{Strategy: "delay", SettleMS: 8000}
	if code, _ := post(t, newDeliverHandler(f, clk), r); code != http.StatusOK {
		t.Fatal(code)
	}
	got := f.verbs()
	if len(got) < 3 || got[1] != "sh" || got[2] != "load-buffer" {
		t.Fatalf("verbs = %v, want has-session, one probe, then load-buffer", got)
	}
}

// TestDeliverSkewedNotAfterStillPastes: an agent clock 120 s ahead (the
// not_after is already past at receipt) uses the receipt bound and pastes.
func TestDeliverSkewedNotAfterStillPastes(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	f := &fakeTmux{}
	r := baseReq(clk.Now().Add(-120 * time.Second))
	if code, resp := post(t, newDeliverHandler(f, clk), r); code != http.StatusOK || resp.Outcome != agentcontrol.OutcomeDelivered {
		t.Fatalf("= %d %+v", code, resp)
	}
}

// TestDeliverLogLineContentFree: the info line carries no text and no key
// names.
func TestDeliverLogLineContentFree(t *testing.T) {
	r := baseReq(time.Now())
	r.Text = strp("SENTINEL-LOG-TEXT")
	r.Op = agentcontrol.OpInterruptPaste
	r.InterruptKeys = []string{"Escape"}
	line := deliverLogLine(&r, deliverResult{status: 200, resp: agentcontrol.DeliverResponse{Outcome: "delivered"}})
	for _, bad := range []string{"SENTINEL-LOG-TEXT", "Escape", "Enter"} {
		if strings.Contains(line, bad) {
			t.Fatalf("log line %q contains %q", line, bad)
		}
	}
}

// TestDeliverHardStopAfterInterrupt (review S-A-01): the hard stop is
// checked before every paste, also after interrupt keys were sent. A stop
// that passes after the interrupt (during the interrupt itself or the
// buffer load) means no paste and a stored partial, never a 408 the hub
// would unbind on; the replay is partial too.
func TestDeliverHardStopAfterInterrupt(t *testing.T) {
	for _, slowVerb := range []string{"send-keys", "load-buffer"} {
		t.Run("slow "+slowVerb, func(t *testing.T) {
			clk := &fakeClock{t: time.Unix(1_000_000, 0)}
			f := &fakeTmux{}
			f.onVerb = func(verb string) {
				if verb == slowVerb {
					clk.Advance(93 * time.Second)
				}
			}
			h := newDeliverHandler(f, clk)
			r := baseReq(clk.Now())
			r.Op = agentcontrol.OpInterruptPaste
			r.InterruptKeys = []string{"C-c"}
			code, resp := post(t, h, r)
			if code != http.StatusOK || resp.Outcome != agentcontrol.OutcomePartial || resp.Stage != agentcontrol.StagePaste {
				t.Fatalf("= %d %+v, want 200 partial paste", code, resp)
			}
			if slices.Contains(f.verbs(), "paste-buffer") {
				t.Fatalf("pasted after the hard stop: %v", f.verbs())
			}
			n := len(f.verbs())
			if code, resp := post(t, h, r); code != http.StatusOK || !resp.Replayed || resp.Outcome != agentcontrol.OutcomePartial {
				t.Fatalf("replay = %d %+v", code, resp)
			}
			if len(f.verbs()) != n {
				t.Fatal("replay ran tmux")
			}
		})
	}
	// Control: expiry before anything visible stays 408 (TestDeliverReadinessPastHardStop).
}

// TestDeliverSizeLimits (review R3): over a size limit is 413 too_large,
// which the hub and broker treat as "use exec", not the permanent 400 of a
// malformed request; nothing runs.
func TestDeliverSizeLimits(t *testing.T) {
	now := time.Now()
	for name, tc := range map[string]struct {
		mod   func(r *agentcontrol.DeliverRequest)
		field string
	}{
		"text over 512 KiB": {func(r *agentcontrol.DeliverRequest) {
			r.Text = strp(strings.Repeat("x", agentcontrol.MaxDeliverText+1))
		}, agentcontrol.FieldText},
		"65 message ids": {func(r *agentcontrol.DeliverRequest) { r.MessageIDs = manyIDs(65) }, agentcontrol.FieldMessageIDs},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeTmux{}
			r := baseReq(now)
			tc.mod(&r)
			code, resp := post(t, newDeliverHandler(f, nil), r)
			if code != http.StatusRequestEntityTooLarge || resp.Outcome != agentcontrol.OutcomeTooLarge || resp.Field != tc.field {
				t.Fatalf("= %d %+v, want 413 too_large %s", code, resp, tc.field)
			}
			if len(f.verbs()) != 0 {
				t.Fatalf("ran %v", f.verbs())
			}
		})
	}
}
