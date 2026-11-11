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

package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/GoogleCloudPlatform/scion/pkg/util/asyncwrite"
)

// neverTimer never fires, so no write budget expires during these tests
// regardless of scheduling; timeout behaviour is covered in asyncwrite.
type neverTimer struct{}

func (neverTimer) Stop() bool { return true }

func neverAfterFunc(time.Duration, func()) asyncwrite.Timer { return neverTimer{} }

// captureInner renders records as JSON and reports each Handle on out. If
// gate is non-nil, Handle waits on it before rendering.
type captureInner struct {
	mu      sync.Mutex
	buf     *bytes.Buffer
	json    slog.Handler
	gate    chan struct{}
	entered chan struct{}
	out     chan captured
	level   slog.Level
}

type captured struct {
	line        string
	span        trace.SpanContext
	ctx         context.Context
	errAtWrite  error // ctx.Err() observed inside Handle
	hasDeadline bool
}

func newCaptureInner(gate chan struct{}) *captureInner {
	buf := &bytes.Buffer{}
	return &captureInner{
		buf:     buf,
		json:    slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}),
		gate:    gate,
		entered: make(chan struct{}, 64),
		out:     make(chan captured, 64),
	}
}

type captureView struct {
	c    *captureInner
	json slog.Handler
}

func (c *captureInner) handler() slog.Handler { return &captureView{c: c, json: c.json} }

func (v *captureView) Enabled(_ context.Context, l slog.Level) bool { return l >= v.c.level }
func (v *captureView) Handle(ctx context.Context, r slog.Record) error {
	v.c.entered <- struct{}{}
	if v.c.gate != nil {
		<-v.c.gate
	}
	v.c.mu.Lock()
	v.c.buf.Reset()
	err := v.json.Handle(ctx, r)
	line := v.c.buf.String()
	v.c.mu.Unlock()
	_, hasDeadline := ctx.Deadline()
	v.c.out <- captured{line: line, span: trace.SpanContextFromContext(ctx), ctx: ctx,
		errAtWrite: ctx.Err(), hasDeadline: hasDeadline}
	return err
}
func (v *captureView) WithAttrs(as []slog.Attr) slog.Handler {
	return &captureView{c: v.c, json: v.json.WithAttrs(as)}
}
func (v *captureView) WithGroup(n string) slog.Handler {
	return &captureView{c: v.c, json: v.json.WithGroup(n)}
}

func newTestAsync(t *testing.T, gate chan struct{}) (*AsyncHandler, *asyncwrite.Writer[AsyncRecord], *captureInner) {
	t.Helper()
	w, err := NewAsyncWriter(asyncwrite.Config{Name: "audit", AfterFunc: neverAfterFunc})
	if err != nil {
		t.Fatal(err)
	}
	// If the test failed (possibly before releasing its gate), Close with a
	// cancelled context so Cleanup returns instead of waiting forever on a
	// parked worker; neverAfterFunc also disables the drain timer.
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		if t.Failed() {
			cancel()
		}
		defer cancel()
		_ = w.Close(ctx)
	})
	c := newCaptureInner(gate)
	return NewAsyncHandler(c.handler(), w), w, c
}

func decode(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return m
}

func rec(msg string, attrs ...slog.Attr) slog.Record {
	r := slog.NewRecord(time.Unix(1_700_000_000, 0), slog.LevelInfo, msg, 0)
	r.AddAttrs(attrs...)
	return r
}

func TestAsyncHandler_WritesSupportedKindsAndViews(t *testing.T) {
	h, w, c := newTestAsync(t, nil)
	view := h.WithAttrs([]slog.Attr{slog.String("component", "hub")}).WithGroup("g")
	r := rec("scion.audit",
		slog.String("s", "v"), slog.Int64("i", -3), slog.Uint64("u", 4), slog.Float64("f", 1.5),
		slog.Bool("b", true), slog.Duration("d", time.Second), slog.Time("t", time.Unix(5, 0)),
		slog.Group("grp", slog.String("k", "x")), slog.Any("list", []string{"a", "b"}),
	)
	if err := view.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle = %v", err)
	}
	got := decode(t, (<-c.out).line)
	if got["component"] != "hub" {
		t.Fatalf("WithAttrs not preserved: %v", got)
	}
	g, ok := got["g"].(map[string]any)
	if !ok || g["s"] != "v" || g["b"] != true || g["grp"].(map[string]any)["k"] != "x" {
		t.Fatalf("WithGroup not preserved: %v", got)
	}
	if list := g["list"].([]any); len(list) != 2 || list[0] != "a" {
		t.Fatalf("[]string not preserved: %v", g["list"])
	}
	if s := w.Snapshot(); s.Enqueued != 1 {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestAsyncHandler_EnabledDelegates(t *testing.T) {
	h, _, c := newTestAsync(t, nil)
	c.level = slog.LevelWarn
	if h.Enabled(context.Background(), slog.LevelInfo) || !h.Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("Enabled does not delegate to inner")
	}
}

// P1-2: the queued record is a deep copy; caller mutation after Handle
// returns does not change the output.
func TestAsyncHandler_RecordIsDeepCopied(t *testing.T) {
	gate := make(chan struct{})
	h, _, c := newTestAsync(t, gate)
	list := []string{"orig-0", "orig-1"}
	members := []slog.Attr{slog.String("m", "orig-m")}
	r := rec("scion.audit", slog.Any("list", list), slog.Attr{Key: "grp", Value: slog.GroupValue(members...)})
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	// Mutate every piece of caller-owned storage while the record is queued.
	list[0] = "MUTATED"
	members[0] = slog.String("m", "MUTATED")
	r.AddAttrs(slog.String("late", "MUTATED"))
	close(gate)
	line := (<-c.out).line
	if strings.Contains(line, "MUTATED") {
		t.Fatalf("caller mutation leaked into queued record: %s", line)
	}
	if !strings.Contains(line, "orig-0") || !strings.Contains(line, "orig-m") {
		t.Fatalf("original values missing: %s", line)
	}
}

// P1-2: the trace span context survives caller cancellation; no other
// caller context value is retained.
type ctxKey struct{}

func TestAsyncHandler_SpanContextSurvivesCancellationAndNothingElseRetained(t *testing.T) {
	gate := make(chan struct{})
	h, _, c := newTestAsync(t, gate)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
	})
	ctx, cancel := context.WithCancel(trace.ContextWithSpanContext(context.Background(), sc))
	ctx = context.WithValue(ctx, ctxKey{}, "request-secret")
	if err := h.Handle(ctx, rec("scion.audit", slog.String("k", "v"))); err != nil {
		t.Fatal(err)
	}
	cancel()
	close(gate)
	got := <-c.out
	if !got.span.Equal(sc) {
		t.Fatalf("span context = %v, want %v", got.span, sc)
	}
	if errors.Is(got.errAtWrite, context.Canceled) {
		t.Fatalf("write context inherited caller cancellation: %v", got.errAtWrite)
	}
	if got.ctx.Value(ctxKey{}) != nil {
		t.Fatal("caller context value retained")
	}
}

// F1: the caller's TraceState (request-supplied, variable size) is never
// retained; only TraceID, SpanID, TraceFlags and Remote are.
func TestAsyncHandler_SpanSnapshotDropsTraceState(t *testing.T) {
	gate := make(chan struct{})
	h, _, c := newTestAsync(t, gate)
	ts, err := trace.ParseTraceState("vendor1=" + strings.Repeat("a", 200) + ",vendor2=b")
	if err != nil {
		t.Fatal(err)
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0xa, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     trace.SpanID{0xb, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
		TraceState: ts,
		Remote:     true,
	})
	if sc.TraceState().Len() != 2 {
		t.Fatalf("setup: trace state len = %d", sc.TraceState().Len())
	}
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), sc)
	if err := h.Handle(ctx, rec("scion.audit")); err != nil {
		t.Fatal(err)
	}
	close(gate)
	got := (<-c.out).span
	if got.TraceID() != sc.TraceID() || got.SpanID() != sc.SpanID() ||
		got.TraceFlags() != sc.TraceFlags() || got.IsRemote() != sc.IsRemote() {
		t.Fatalf("span identity changed: got %v want %v", got, sc)
	}
	if got.TraceState().Len() != 0 {
		t.Fatalf("TraceState retained: %q", got.TraceState().String())
	}
}

// F3: the write context carries the worker's budget deadline.
func TestAsyncHandler_WriteContextHasBudgetDeadline(t *testing.T) {
	h, _, c := newTestAsync(t, nil)
	if err := h.Handle(context.Background(), rec("m")); err != nil {
		t.Fatal(err)
	}
	if !(<-c.out).hasDeadline {
		t.Fatal("write context has no budget deadline")
	}
}

func TestAsyncHandler_NoSpanContextWithoutValidSpan(t *testing.T) {
	h, _, c := newTestAsync(t, nil)
	if err := h.Handle(context.Background(), rec("m")); err != nil {
		t.Fatal(err)
	}
	if got := <-c.out; got.span.IsValid() {
		t.Fatal("unexpected span context")
	}
}

// panicValuer fails the test if any caller code runs.
type panicValuer struct{ t *testing.T }

func (p panicValuer) LogValue() slog.Value {
	p.t.Error("LogValue executed on the caller path")
	return slog.StringValue("x")
}

type panicStringer struct{ t *testing.T }

func (p panicStringer) String() string {
	p.t.Error("String executed on the caller path")
	return "x"
}

func TestAsyncHandler_RejectsUnsupportedWithoutRunningCallerCode(t *testing.T) {
	h, w, _ := newTestAsync(t, nil)
	cases := map[string]slog.Attr{
		"logvaluer":     slog.Any("v", panicValuer{t}),
		"stringer":      slog.Any("v", panicStringer{t}),
		"map":           slog.Any("v", map[string]string{"a": "b"}),
		"bytes":         slog.Any("v", []byte("x")),
		"error":         slog.Any("v", errors.New("x")),
		"nested-valuer": slog.Group("g", slog.Any("v", panicValuer{t})),
	}
	n := 0
	for name, a := range cases {
		if err := h.Handle(context.Background(), rec("m", a)); !errors.Is(err, ErrAsyncUnsupported) {
			t.Errorf("%s: Handle = %v, want ErrAsyncUnsupported", name, err)
		}
		n++
	}
	if s := w.Snapshot(); s.DroppedUnsupported != uint64(n) || s.Enqueued != 0 {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestAsyncHandler_LimitsRejectAsOversize(t *testing.T) {
	big := func(n int) string { return strings.Repeat("x", n) }
	many := func(n int) []slog.Attr {
		as := make([]slog.Attr, n)
		for i := range as {
			as[i] = slog.Bool("b", true)
		}
		return as
	}
	items := func(n, size int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = big(size)
		}
		return out
	}
	type tc struct {
		msg   string
		attrs []slog.Attr
		ok    bool
	}
	cases := map[string]tc{
		"message at limit":   {msg: big(64), ok: true},
		"message over":       {msg: big(65)},
		"key at limit":       {msg: "m", attrs: []slog.Attr{slog.Bool(big(64), true)}, ok: true},
		"key over":           {msg: "m", attrs: []slog.Attr{slog.Bool(big(65), true)}},
		"group key over":     {msg: "m", attrs: []slog.Attr{slog.Group(big(65), slog.Bool("b", true))}},
		"string at limit":    {msg: "m", attrs: []slog.Attr{slog.String("s", big(1024))}, ok: true},
		"string over":        {msg: "m", attrs: []slog.Attr{slog.String("s", big(1025))}},
		"items at limit":     {msg: "m", attrs: []slog.Attr{slog.Any("l", items(32, 1))}, ok: true},
		"items over":         {msg: "m", attrs: []slog.Attr{slog.Any("l", items(33, 1))}},
		"item at limit":      {msg: "m", attrs: []slog.Attr{slog.Any("l", items(1, 256))}, ok: true},
		"item over":          {msg: "m", attrs: []slog.Attr{slog.Any("l", items(1, 257))}},
		"attrs at limit":     {msg: "m", attrs: many(64), ok: true},
		"attrs over":         {msg: "m", attrs: many(65)},
		"group members over": {msg: "m", attrs: []slog.Attr{slog.Group("g", anySlice(many(64))...)}},
		"depth at limit": {msg: "m", attrs: []slog.Attr{slog.Group("a", slog.Group("b",
			slog.Group("c", slog.Bool("x", true))))}, ok: true},
		"depth over": {msg: "m", attrs: []slog.Attr{slog.Group("a", slog.Group("b",
			slog.Group("c", slog.Group("d", slog.Bool("x", true)))))}},
		"total over": {msg: "m", attrs: []slog.Attr{
			slog.String("a", big(1024)), slog.String("b", big(1024)),
			slog.String("c", big(1024)), slog.String("d", big(1024)),
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h, w, out := newTestAsync(t, nil)
			err := h.Handle(context.Background(), rec(c.msg, c.attrs...))
			s := w.Snapshot()
			if c.ok {
				if err != nil {
					t.Fatalf("Handle = %v", err)
				}
				<-out.out
				return
			}
			if !errors.Is(err, ErrAsyncOversize) {
				t.Fatalf("Handle = %v, want ErrAsyncOversize", err)
			}
			if s.DroppedOversize != 1 || s.Enqueued != 0 {
				t.Fatalf("snapshot = %+v", s)
			}
		})
	}
}

func anySlice(as []slog.Attr) []any {
	out := make([]any, len(as))
	for i, a := range as {
		out[i] = a
	}
	return out
}

func TestAsyncHandler_AccountedBytesAreQueuedBytesAndReturnToZero(t *testing.T) {
	gate := make(chan struct{})
	h, w, c := newTestAsync(t, gate)
	view := h.WithAttrs([]slog.Attr{slog.String("ab", "cdef")}) // 2 + 4
	// message 11 + "k"+"vv" 3 + "n" + 16 + "l" + "x"+"yz" 4
	r := rec("scion.audit", slog.String("k", "vv"), slog.Int64("n", 1), slog.Any("l", []string{"x", "yz"}))
	if err := view.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := view.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	// The first item is in flight (uncharged), the second is queued.
	want := int64(6 + 11 + 3 + 1 + 16 + 4)
	<-c.entered
	if s := w.Snapshot(); s.QueuedBytes != want {
		t.Fatalf("queued bytes = %d, want %d", s.QueuedBytes, want)
	}
	close(gate)
	<-c.out
	<-c.out
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := w.Snapshot(); s.QueuedBytes != 0 || s.Written != 2 {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestAsyncHandler_ViewChargesCountAgainstRecordLimits(t *testing.T) {
	h, w, _ := newTestAsync(t, nil)
	pre := make([]slog.Attr, 60)
	for i := range pre {
		pre[i] = slog.Bool("p", true)
	}
	view := h.WithAttrs(pre)
	r := rec("m", slog.Bool("a", true), slog.Bool("b", true), slog.Bool("c", true),
		slog.Bool("d", true), slog.Bool("e", true))
	if err := view.Handle(context.Background(), r); !errors.Is(err, ErrAsyncOversize) {
		t.Fatalf("Handle = %v, want ErrAsyncOversize (60 pre + 5 > 64)", err)
	}
	deep := h.WithGroup("a").WithGroup("b").WithGroup("c")
	if err := deep.Handle(context.Background(), rec("m", slog.Group("d", slog.Bool("x", true)))); !errors.Is(err, ErrAsyncOversize) {
		t.Fatalf("Handle = %v, want ErrAsyncOversize (view depth)", err)
	}
	if s := w.Snapshot(); s.DroppedOversize != 2 {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestAsyncHandler_InvalidViewsReject(t *testing.T) {
	h, w, _ := newTestAsync(t, nil)
	views := []slog.Handler{
		h.WithAttrs([]slog.Attr{slog.Any("v", panicValuer{t})}),
		h.WithAttrs([]slog.Attr{slog.String(strings.Repeat("k", 65), "v")}),
		h.WithGroup(strings.Repeat("g", 65)),
		h.WithGroup("a").WithGroup("b").WithGroup("c").WithGroup("d"),
		h.WithAttrs([]slog.Attr{slog.Any("v", map[int]int{})}).WithAttrs([]slog.Attr{slog.String("ok", "ok")}),
	}
	for i, v := range views {
		if err := v.Handle(context.Background(), rec("m")); !errors.Is(err, ErrAsyncUnsupported) {
			t.Errorf("view %d: Handle = %v, want ErrAsyncUnsupported", i, err)
		}
	}
	if s := w.Snapshot(); s.DroppedUnsupported != uint64(len(views)) || s.Enqueued != 0 {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestAsyncHandler_ViewAttrsAreCloned(t *testing.T) {
	gate := make(chan struct{})
	h, _, c := newTestAsync(t, gate)
	list := []string{"orig"}
	view := h.WithAttrs([]slog.Attr{slog.Any("l", list)})
	list[0] = "MUTATED"
	if err := view.Handle(context.Background(), rec("m")); err != nil {
		t.Fatal(err)
	}
	close(gate)
	if line := (<-c.out).line; strings.Contains(line, "MUTATED") || !strings.Contains(line, "orig") {
		t.Fatalf("view attrs not cloned: %s", line)
	}
}

func TestAsyncHandler_QueueFullAndClosedErrors(t *testing.T) {
	gate := make(chan struct{})
	w, err := NewAsyncWriter(asyncwrite.Config{Name: "audit", Capacity: 1, AfterFunc: neverAfterFunc})
	if err != nil {
		t.Fatal(err)
	}
	c := newCaptureInner(gate)
	h := NewAsyncHandler(c.handler(), w)
	if err := h.Handle(context.Background(), rec("m")); err != nil {
		t.Fatal(err)
	}
	<-c.entered // first record in flight; worker blocked on gate
	if err := h.Handle(context.Background(), rec("m")); err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(context.Background(), rec("m")); !errors.Is(err, asyncwrite.ErrFull) {
		t.Fatalf("Handle = %v, want ErrFull", err)
	}
	close(gate)
	<-c.out
	<-c.out
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(context.Background(), rec("m")); !errors.Is(err, asyncwrite.ErrClosed) {
		t.Fatalf("Handle = %v, want ErrClosed", err)
	}
	s := w.Snapshot()
	if s.DroppedFull != 1 || s.DroppedClosed != 1 || s.Written != 2 {
		t.Fatalf("snapshot = %+v", s)
	}
}

// The audit sink's shape (auditevent.SlogSink) fits within the limits.
func TestAsyncHandler_AuditShapedRecordAccepted(t *testing.T) {
	h, _, c := newTestAsync(t, nil)
	r := rec("scion.audit",
		slog.Int("schema_version", 1),
		slog.String("event_id", strings.Repeat("e", 36)),
		slog.String("occurred_at", time.Now().UTC().Format(time.RFC3339Nano)),
		slog.String("family", "authorization"), slog.String("action", "decide"),
		slog.String("phase", "decision"), slog.String("outcome", "allowed"),
		slog.String("severity", "info"), slog.String("correlation_id", strings.Repeat("c", 128)),
		slog.Group("request", slog.String("id", strings.Repeat("r", 128)), slog.String("route", "/api/v1/projects/{id}")),
		slog.Group("principal", slog.String("kind", "user"), slog.String("id", strings.Repeat("u", 128))),
		slog.Group("credential", slog.String("kind", "user_token"), slog.String("id", strings.Repeat("t", 128)),
			slog.String("boundary_kind", "project"), slog.String("boundary_project_id", strings.Repeat("p", 128))),
		slog.Group("resource", slog.String("kind", "project"), slog.String("id", strings.Repeat("p", 128))),
		slog.Group("payload", slog.String("permission_id", strings.Repeat("q", 128)),
			slog.String("permission", strings.Repeat("q", 128)), slog.String("reason", strings.Repeat("z", 256)),
			slog.String("sampled", "true"), slog.Any("denied_by", []string{"policy"})),
	)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("audit-shaped record rejected: %v", err)
	}
	<-c.out
}

// N1: pass 2 does not trust pass 1. A caller that mutates shared attr
// storage between the passes gets the record rejected as unsupported:
// no panic, nothing unvalidated queued.
func TestAsyncHandler_Pass2RejectsConcurrentMutation(t *testing.T) {
	cases := map[string]func(members []slog.Attr, list []string){
		"kind swapped to LogValuer": func(m []slog.Attr, _ []string) { m[0] = slog.Any("m", panicValuer{t}) },
		"kind swapped to map":       func(m []slog.Attr, _ []string) { m[0] = slog.Any("m", map[string]int{}) },
		"string grew":               func(m []slog.Attr, _ []string) { m[0] = slog.String("m", "a-longer-value") },
		"list item grew":            func(_ []slog.Attr, l []string) { l[0] = "a-longer-item" },
		"key grew":                  func(m []slog.Attr, _ []string) { m[0] = slog.String("mm", "v") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h, w, _ := newTestAsync(t, nil)
			members := []slog.Attr{slog.String("m", "v")}
			list := []string{"x"}
			h.betweenPasses = func() { mutate(members, list) }
			r := rec("scion.audit", slog.Attr{Key: "g", Value: slog.GroupValue(members...)}, slog.Any("l", list))
			if err := h.Handle(context.Background(), r); !errors.Is(err, ErrAsyncUnsupported) {
				t.Fatalf("Handle = %v, want ErrAsyncUnsupported", err)
			}
			if s := w.Snapshot(); s.DroppedUnsupported != 1 || s.Enqueued != 0 {
				t.Fatalf("snapshot = %+v", s)
			}
		})
	}
}

// N1: the pass-2 cloner fails (never panics) outside the accepted kinds and
// recomputes exactly the pass-1 accounting for accepted values.
func TestAsyncHandler_ClonerMatchesPass1Accounting(t *testing.T) {
	attrs := []slog.Attr{
		slog.String("s", "abc"), slog.Int64("i", 1), slog.Bool("b", true),
		slog.Group("g", slog.String("k", "vv"), slog.Any("l", []string{"x", "yz"})),
	}
	var acc accounting
	cl := cloner{}
	for _, a := range attrs {
		if err := acc.attr(a, 0); err != nil {
			t.Fatal(err)
		}
		if _, ok := cl.attr(a); !ok {
			t.Fatalf("cloner rejected accepted attr %v", a)
		}
	}
	if !cl.ok() || cl.bytes != acc.bytes || cl.attrs != acc.attrs {
		t.Fatalf("cloner bytes/attrs = %d/%d, pass 1 = %d/%d", cl.bytes, cl.attrs, acc.bytes, acc.attrs)
	}
	for _, bad := range []slog.Attr{
		slog.Any("v", panicValuer{t}), slog.Any("v", struct{ N int }{42}), slog.Any("v", []int{1}),
		slog.Group("g", slog.Any("v", panicStringer{t})),
	} {
		c := cloner{}
		if _, ok := c.attr(bad); ok || c.ok() {
			t.Fatalf("cloner accepted %v", bad)
		}
	}
}
