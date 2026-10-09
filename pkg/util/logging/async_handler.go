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
// cloned values: no caller context, no shared attr storage.
type AsyncRecord struct {
	handler slog.Handler
	record  slog.Record
	span    trace.SpanContext
}

// NewAsyncWriter creates the asyncwrite.Writer that drains AsyncRecords into
// their (view-derived) inner handlers. Each write runs with a fresh
// background context carrying only the caller's snapshotted span context.
func NewAsyncWriter(cfg asyncwrite.Config) (*asyncwrite.Writer[AsyncRecord], error) {
	return asyncwrite.New(cfg, writeAsyncRecord)
}

func writeAsyncRecord(item AsyncRecord) error {
	ctx := context.Background()
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

	// Pass 2: clone into a fresh record; never r.Clone (shared storage).
	out := slog.NewRecord(r.Time, r.Level, strings.Clone(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(cloneAttr(a))
		return true
	})

	item := AsyncRecord{handler: h.inner, record: out}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		item.span = sc
	}
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
	for i, a := range attrs {
		cloned[i] = cloneAttr(a)
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

// cloneAttr deep-copies an attr already accepted by accounting.attr.
func cloneAttr(a slog.Attr) slog.Attr {
	return slog.Attr{Key: strings.Clone(a.Key), Value: cloneValue(a.Value)}
}

func cloneValue(v slog.Value) slog.Value {
	switch v.Kind() {
	case slog.KindString:
		return slog.StringValue(strings.Clone(v.String()))
	case slog.KindGroup:
		members := v.Group()
		cloned := make([]slog.Attr, len(members))
		for i, m := range members {
			cloned[i] = cloneAttr(m)
		}
		return slog.GroupValue(cloned...)
	case slog.KindAny:
		items := slices.Clone(v.Any().([]string))
		for i := range items {
			items[i] = strings.Clone(items[i])
		}
		return slog.AnyValue(items)
	default: // scalars are values
		return v
	}
}

var _ slog.Handler = (*AsyncHandler)(nil)
