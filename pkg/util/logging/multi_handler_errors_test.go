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
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type failingHandler struct {
	err   error
	calls int
}

func (h *failingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *failingHandler) Handle(context.Context, slog.Record) error {
	h.calls++
	return h.err
}
func (h *failingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *failingHandler) WithGroup(string) slog.Handler      { return h }

// P1-8 (D5): a failing child does not prevent later children from writing,
// and its error is returned joined.
func TestMultiHandler_HandleReturnsJoinedChildErrors(t *testing.T) {
	errA := errors.New("child a failed")
	errB := errors.New("child b failed")
	a := &failingHandler{err: errA}
	var buf bytes.Buffer
	healthy := slog.NewJSONHandler(&buf, nil)
	b := &failingHandler{err: errB}
	m := newMultiHandler(a, healthy, b)

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "hello", 0)
	err := m.Handle(context.Background(), r)
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Fatalf("Handle error = %v, want both child errors joined", err)
	}
	if !strings.Contains(buf.String(), `"msg":"hello"`) {
		t.Fatalf("healthy child not written: %q", buf.String())
	}
	if a.calls != 1 || b.calls != 1 {
		t.Fatalf("calls a=%d b=%d", a.calls, b.calls)
	}
}

func TestMultiHandler_HandleNilWhenAllSucceed(t *testing.T) {
	var b1, b2 bytes.Buffer
	m := newMultiHandler(slog.NewJSONHandler(&b1, nil), slog.NewJSONHandler(&b2, nil))
	if err := m.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "ok", 0)); err != nil {
		t.Fatalf("Handle = %v", err)
	}
	if b1.Len() == 0 || b2.Len() == 0 {
		t.Fatal("children not written")
	}
}

// slog.Logger callers are unaffected: the joined error is discarded.
func TestMultiHandler_LoggerIgnoresJoinedError(t *testing.T) {
	m := newMultiHandler(&failingHandler{err: errors.New("x")})
	slog.New(m).Info("does not panic or surface")
}
