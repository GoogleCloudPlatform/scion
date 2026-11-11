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

package transport

import (
	"context"
	"errors"
	"testing"
)

func TestPipeRoundTripAndClose(t *testing.T) {
	a, b := Pipe(MemoryOptions{Buffer: 4})
	if a.Transport() != Memory {
		t.Fatalf("Transport() = %q", a.Transport())
	}
	for _, m := range []string{"one", "two"} {
		if err := a.WriteFrame([]byte(m)); err != nil {
			t.Fatal(err)
		}
	}
	_ = a.Close()
	// Frames written before close are still delivered, then ErrClosed.
	for _, m := range []string{"one", "two"} {
		got, err := b.ReadFrame()
		if err != nil || string(got) != m {
			t.Fatalf("got %q, %v; want %q", got, err, m)
		}
	}
	if _, err := b.ReadFrame(); !errors.Is(err, ErrClosed) {
		t.Fatalf("read after close: %v", err)
	}
	if err := b.WriteFrame([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close: %v", err)
	}
}

func TestPipeUnbufferedCloseUnblocksWriter(t *testing.T) {
	a, b := Pipe(MemoryOptions{})
	errc := make(chan error, 1)
	go func() { errc <- a.WriteFrame([]byte("blocked")) }()
	_ = b.Close()
	if err := <-errc; !errors.Is(err, ErrClosed) {
		t.Fatalf("blocked write: %v", err)
	}
}

func TestPipeMaxFrame(t *testing.T) {
	a, _ := Pipe(MemoryOptions{Buffer: 1, MaxFrame: 8})
	if err := a.WriteFrame(make([]byte, 9)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

func TestMemoryListener(t *testing.T) {
	l := NewMemoryListener(MemoryOptions{Buffer: 1})
	accepted := make(chan Conn, 1)
	go func() {
		c, err := l.Accept(context.Background())
		if err == nil {
			accepted <- c
		}
	}()
	c, err := l.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := <-accepted
	if err := c.WriteFrame([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadFrame(); err != nil || string(got) != "hi" {
		t.Fatalf("got %q, %v", got, err)
	}
	_ = l.Close()
	if _, err := l.Dial(context.Background()); err == nil {
		t.Fatal("Dial after Close succeeded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Accept(ctx); err == nil {
		t.Fatal("Accept after Close succeeded")
	}
}
