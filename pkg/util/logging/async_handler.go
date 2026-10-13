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
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/GoogleCloudPlatform/scion/pkg/util/asyncwrite"
)

// Per-record limits enforced by AsyncHandler before anything is copied or
// queued. They are sized for the audit envelope emitted by
// auditevent.SlogSink; any record exceeding them is rejected (never
// truncated) and counted as oversize.
const (
	AsyncMaxMessageBytes  = 64
	AsyncMaxKeyBytes      = 64
	AsyncMaxStringBytes   = 1024
	AsyncMaxStringsItems  = 32
	AsyncMaxStringsItem   = 256
	AsyncMaxAttrs         = 64
	AsyncMaxGroupDepth    = 3
	AsyncMaxRecordBytes   = 4 << 10
	asyncScalarAccounting = 16
)

var (
	// ErrAsyncOversize reports a record exceeding AsyncHandler's limits.
	ErrAsyncOversize = errors.New("async log handler: record exceeds limits")
	// ErrAsyncUnsupported reports a record (or handler view) containing a
	// value kind AsyncHandler does not snapshot.
	ErrAsyncUnsupported = errors.New("async log handler: unsupported value kind")
)

// AsyncRecord is the immutable item queued by AsyncHandler. It holds only
// cloned values: no caller context, no shared attr storage. From the caller
// context it keeps only the fixed-size trace identity (TraceID, SpanID,
// TraceFlags, Remote) of a valid span context; the variable-size,
// request-supplied TraceState is deliberately dropped, because it is caller
// memory outside every byte budget and no sink needs it.
type AsyncRecord struct {
	handler slog.Handler
	record  slog.Record
	span    trace.SpanContext
}

// NewAsyncWriter creates the asyncwrite.Writer that drains AsyncRecords into
// their (view-derived) inner handlers. Each write runs with the worker's
// fresh budget context (deadline = WriteBudget, derived from
// context.Background()) carrying only the snapshotted span identity.
func NewAsyncWriter(cfg asyncwrite.Config) (*asyncwrite.Writer[AsyncRecord], error) {
	return asyncwrite.New(cfg, writeAsyncRecord)
}

func writeAsyncRecord(ctx context.Context, item AsyncRecord) error {
	if item.span.IsValid() {
		ctx = trace.ContextWithSpanContext(ctx, item.span)
	}
	return item.handler.Handle(ctx, item.record)
}

// AsyncHandler is a slog.Handler that validates and deep-copies each record
// on the caller goroutine, then enqueues it on a bounded asyncwrite.Writer
// without blocking. It accepts only the value kinds auditevent.SlogSink
// emits (String, Int64, Uint64, Float64, Bool, Duration, Time, Group and a
// concrete []string). It never calls Resolve, LogValue or String on caller
// values, so no caller code runs on the request path.
//
// Handle returns asyncwrite.ErrFull / asyncwrite.ErrClosed on a queue drop,
// ErrAsyncOversize / ErrAsyncUnsupported on a rejected record, and nil once
// the record is queued. It never reports the eventual write outcome; that
// is counted by the writer.
type AsyncHandler struct {
	w     *asyncwrite.Writer[AsyncRecord]
	inner slog.Handler

	// Charges of the view's cloned pre-attrs and group keys, applied to
	// every Handle against the per-record limits.
	preBytes int
	preAttrs int
	depth    int

	// reject marks a view whose WithAttrs/WithGroup input failed
	// validation; every Handle counts unsupported and enqueues nothing.
	reject bool

	// betweenPasses is a test-only hook run after pass 1 succeeds and before
	// pass 2; nil in production.
	betweenPasses func()
}

// NewAsyncHandler wraps inner, queueing records on w.
func NewAsyncHandler(inner slog.Handler, w *asyncwrite.Writer[AsyncRecord]) *AsyncHandler {
	return &AsyncHandler{w: w, inner: inner}
}

// Enabled delegates to the inner handler.
func (h *AsyncHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle validates (pass 1), clones (pass 2) and enqueues r.
func (h *AsyncHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.reject {
		h.w.Reject(asyncwrite.ResultUnsupported)
		return ErrAsyncUnsupported
	}

	// Pass 1: read-only validation using Kind() only.
	acc := accounting{bytes: h.preBytes, attrs: h.preAttrs}
	if len(r.Message) > AsyncMaxMessageBytes {
		h.w.Reject(asyncwrite.ResultOversize)
		return ErrAsyncOversize
	}
	acc.bytes += len(r.Message)
	var verr error
	r.Attrs(func(a slog.Attr) bool {
		verr = acc.attr(a, h.depth)
		return verr == nil
	})
	if verr == nil {
		verr = acc.check()
	}
	if verr != nil {
		h.w.Reject(resultFor(verr))
		return verr
	}

	if h.betweenPasses != nil {
		h.betweenPasses()
	}

	// Pass 2: clone into a fresh record; never r.Clone (shared storage).
	// The record's attr storage is shared with the caller, so pass 2 does
	// not trust pass 1: it re-checks every kind and recomputes the
	// accounting. Any divergence (only possible if the caller mutated the
	// attrs concurrently) rejects the record as unsupported; nothing
	// unvalidated is ever queued and nothing panics.
	out := slog.NewRecord(r.Time, r.Level, strings.Clone(r.Message), r.PC)
	cl := cloner{bytes: h.preBytes + len(r.Message), attrs: h.preAttrs}
	r.Attrs(func(a slog.Attr) bool {
		c, ok := cl.attr(a)
		if !ok {
			return false
		}
		out.AddAttrs(c)
		return true
	})
	if !cl.ok() || cl.bytes != acc.bytes || cl.attrs != acc.attrs {
		h.w.Reject(asyncwrite.ResultUnsupported)
		return ErrAsyncUnsupported
	}

	item := AsyncRecord{handler: h.inner, record: out}
	item.span = snapshotSpan(trace.SpanContextFromContext(ctx))
	return h.w.TryEnqueue(item, int64(acc.bytes))
}

// WithAttrs validates and clones attrs at view creation. On failure it
// returns a rejecting view (slog.Handler.WithAttrs cannot return an error).
func (h *AsyncHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if h.reject || len(attrs) == 0 {
		return h
	}
	acc := accounting{bytes: h.preBytes, attrs: h.preAttrs}
	for _, a := range attrs {
		if err := acc.attr(a, h.depth); err != nil {
			return h.rejecting()
		}
	}
	if acc.check() != nil {
		return h.rejecting()
	}
	cloned := make([]slog.Attr, len(attrs))
	cl := cloner{bytes: h.preBytes, attrs: h.preAttrs}
	for i, a := range attrs {
		c, ok := cl.attr(a)
		if !ok {
			return h.rejecting()
		}
		cloned[i] = c
	}
	if !cl.ok() || cl.bytes != acc.bytes || cl.attrs != acc.attrs {
		return h.rejecting()
	}
	return &AsyncHandler{
		w:        h.w,
		inner:    h.inner.WithAttrs(cloned),
		preBytes: acc.bytes,
		preAttrs: acc.attrs,
		depth:    h.depth,
	}
}

// WithGroup validates the group key at view creation. On failure it returns
// a rejecting view.
func (h *AsyncHandler) WithGroup(name string) slog.Handler {
	if h.reject || name == "" {
		return h
	}
	if len(name) > AsyncMaxKeyBytes || h.depth+1 > AsyncMaxGroupDepth ||
		h.preBytes+len(name) > AsyncMaxRecordBytes {
		return h.rejecting()
	}
	return &AsyncHandler{
		w:        h.w,
		inner:    h.inner.WithGroup(strings.Clone(name)),
		preBytes: h.preBytes + len(name),
		preAttrs: h.preAttrs,
		depth:    h.depth + 1,
	}
}

// snapshotSpan keeps only the fixed-size identity of a valid span context.
func snapshotSpan(sc trace.SpanContext) trace.SpanContext {
	if !sc.IsValid() {
		return trace.SpanContext{}
	}
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    sc.TraceID(),
		SpanID:     sc.SpanID(),
		TraceFlags: sc.TraceFlags(),
		Remote:     sc.IsRemote(),
	})
}

func (h *AsyncHandler) rejecting() *AsyncHandler {
	return &AsyncHandler{w: h.w, inner: h.inner, reject: true}
}

func resultFor(err error) asyncwrite.Result {
	if errors.Is(err, ErrAsyncUnsupported) {
		return asyncwrite.ResultUnsupported
	}
	return asyncwrite.ResultOversize
}

// accounting tallies pass-1 charges.
type accounting struct {
	bytes int
	attrs int
}

func (acc *accounting) check() error {
	if acc.attrs > AsyncMaxAttrs || acc.bytes > AsyncMaxRecordBytes {
		return ErrAsyncOversize
	}
	return nil
}

// attr validates one attr at group depth using only Value.Kind() and, for
// KindAny, a type assertion to []string. depth is the number of enclosing
// groups.
func (acc *accounting) attr(a slog.Attr, depth int) error {
	acc.attrs++
	if acc.attrs > AsyncMaxAttrs {
		return ErrAsyncOversize
	}
	if len(a.Key) > AsyncMaxKeyBytes {
		return ErrAsyncOversize
	}
	acc.bytes += len(a.Key)
	v := a.Value
	switch v.Kind() {
	case slog.KindString:
		s := v.String() // KindString: returns the stored string, runs no caller code
		if len(s) > AsyncMaxStringBytes {
			return ErrAsyncOversize
		}
		acc.bytes += len(s)
	case slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindBool,
		slog.KindDuration, slog.KindTime:
		acc.bytes += asyncScalarAccounting
	case slog.KindGroup:
		if depth+1 > AsyncMaxGroupDepth {
			return ErrAsyncOversize
		}
		for _, m := range v.Group() {
			if err := acc.attr(m, depth+1); err != nil {
				return err
			}
		}
	case slog.KindAny:
		items, ok := v.Any().([]string)
		if !ok {
			return ErrAsyncUnsupported
		}
		if len(items) > AsyncMaxStringsItems {
			return ErrAsyncOversize
		}
		for _, it := range items {
			if len(it) > AsyncMaxStringsItem {
				return ErrAsyncOversize
			}
			acc.bytes += len(it)
		}
	default: // KindLogValuer and anything unknown
		return ErrAsyncUnsupported
	}
	if acc.bytes > AsyncMaxRecordBytes {
		return ErrAsyncOversize
	}
	return nil
}

// cloner is pass 2: it deep-copies attrs while re-checking each kind
// against the accepted set (checked assertions only) and recomputing the
// pass-1 accounting, so the caller can compare the two totals.
type cloner struct {
	bytes  int
	attrs  int
	failed bool
}

func (c *cloner) ok() bool { return !c.failed }

func (c *cloner) attr(a slog.Attr) (slog.Attr, bool) {
	c.attrs++
	c.bytes += len(a.Key)
	if len(a.Key) > AsyncMaxKeyBytes {
		c.failed = true
		return slog.Attr{}, false
	}
	v, ok := c.value(a.Value)
	if !ok {
		c.failed = true
		return slog.Attr{}, false
	}
	return slog.Attr{Key: strings.Clone(a.Key), Value: v}, true
}

func (c *cloner) value(v slog.Value) (slog.Value, bool) {
	switch v.Kind() {
	case slog.KindString:
		s := v.String()
		c.bytes += len(s)
		if len(s) > AsyncMaxStringBytes {
			return slog.Value{}, false
		}
		return slog.StringValue(strings.Clone(s)), true
	case slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindBool,
		slog.KindDuration, slog.KindTime:
		c.bytes += asyncScalarAccounting
		return v, true // scalars are values
	case slog.KindGroup:
		members := v.Group()
		cloned := make([]slog.Attr, len(members))
		for i, m := range members {
			cm, ok := c.attr(m)
			if !ok {
				return slog.Value{}, false
			}
			cloned[i] = cm
		}
		return slog.GroupValue(cloned...), true
	case slog.KindAny:
		src, ok := v.Any().([]string)
		if !ok || len(src) > AsyncMaxStringsItems {
			return slog.Value{}, false
		}
		items := slices.Clone(src)
		for i := range items {
			if len(items[i]) > AsyncMaxStringsItem {
				return slog.Value{}, false
			}
			c.bytes += len(items[i])
			items[i] = strings.Clone(items[i])
		}
		return slog.AnyValue(items), true
	default: // KindLogValuer and anything unknown
		return slog.Value{}, false
	}
}

var _ slog.Handler = (*AsyncHandler)(nil)
