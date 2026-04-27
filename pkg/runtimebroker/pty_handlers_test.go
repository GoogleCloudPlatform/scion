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

package runtimebroker

import (
	"context"
	"testing"
	"time"

	"k8s.io/client-go/tools/remotecommand"
)

func TestWaitForTmuxSession_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := waitForTmuxSession(ctx, "false", "nonexistent-container", "", "scion", nil, nil)
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
}

func TestWaitForTmuxSession_TimesOut(t *testing.T) {
	// Use a very short timeout to test the timeout path quickly.
	// "false" always exits with code 1, simulating tmux has-session failure.
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := waitForTmuxSession(ctx, "false", "nonexistent-container", "", "scion", nil, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed < 500*time.Millisecond {
		t.Errorf("expected to wait at least 500ms before timing out, got %v", elapsed)
	}
}

func TestWaitForTmuxSession_SucceedsImmediately(t *testing.T) {
	// "true" always exits with code 0, simulating tmux has-session success.
	// We pass extra args that "true" ignores.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := waitForTmuxSession(ctx, "true", "any-container", "", "scion", nil, nil)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	// First poll is at 500ms, so it should complete around that time
	if elapsed > 2*time.Second {
		t.Errorf("expected quick completion, took %v", elapsed)
	}
}

func TestActiveWindowOSC(t *testing.T) {
	tests := []struct {
		name     string
		window   string
		expected string
	}{
		{"agent window", "agent", "\033]7337;tmuxwindow=agent\007"},
		{"shell window", "shell", "\033]7337;tmuxwindow=shell\007"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(activeWindowOSC(tc.window))
			if got != tc.expected {
				t.Errorf("activeWindowOSC(%q) = %q, want %q", tc.window, got, tc.expected)
			}
		})
	}
}

func TestQueryTmuxActiveWindow_CommandFails(t *testing.T) {
	// "false" always exits 1, simulating tmux not available
	result := queryTmuxActiveWindow(context.Background(), "false", "test-container", "scion")
	if result != "" {
		t.Errorf("expected empty string on failure, got %q", result)
	}
}

func TestQueryTmuxActiveWindow_ContextTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	result := queryTmuxActiveWindow(ctx, "echo", "test-container", "scion")
	if result != "" {
		t.Errorf("expected empty string on cancelled context, got %q", result)
	}
}

func TestK8sSizeQueue_ReturnsInitialSize(t *testing.T) {
	q := &k8sSizeQueue{
		resizeCh: make(chan [2]int, 1),
		closeCh:  make(chan struct{}),
		ctx:      context.Background(),
		initial:  &remotecommand.TerminalSize{Width: 120, Height: 40},
	}

	size := q.Next()
	if size == nil {
		t.Fatal("expected initial size, got nil")
	}
	if size.Width != 120 || size.Height != 40 {
		t.Errorf("expected 120x40, got %dx%d", size.Width, size.Height)
	}

	// initial should be consumed
	if q.initial != nil {
		t.Error("expected initial to be nil after first call")
	}
}

func TestK8sSizeQueue_ReturnsResizeEvents(t *testing.T) {
	resizeCh := make(chan [2]int, 1)
	q := &k8sSizeQueue{
		resizeCh: resizeCh,
		closeCh:  make(chan struct{}),
		ctx:      context.Background(),
		initial:  nil, // no initial size
	}

	resizeCh <- [2]int{200, 50}

	size := q.Next()
	if size == nil {
		t.Fatal("expected resize event, got nil")
	}
	if size.Width != 200 || size.Height != 50 {
		t.Errorf("expected 200x50, got %dx%d", size.Width, size.Height)
	}
}

func TestK8sSizeQueue_ReturnsNilOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	q := &k8sSizeQueue{
		resizeCh: make(chan [2]int),
		closeCh:  make(chan struct{}),
		ctx:      ctx,
		initial:  nil,
	}

	size := q.Next()
	if size != nil {
		t.Errorf("expected nil on cancelled context, got %+v", size)
	}
}

func TestK8sSizeQueue_ReturnsNilOnClose(t *testing.T) {
	closeCh := make(chan struct{})
	close(closeCh)

	q := &k8sSizeQueue{
		resizeCh: make(chan [2]int),
		closeCh:  closeCh,
		ctx:      context.Background(),
		initial:  nil,
	}

	size := q.Next()
	if size != nil {
		t.Errorf("expected nil on close, got %+v", size)
	}
}

func TestSanitizeExecUser(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty falls back", in: "", want: "scion"},
		{name: "valid scion", in: "scion", want: "scion"},
		{name: "valid root", in: "root", want: "root"},
		{name: "valid alphanumeric with hyphen", in: "agent-user_1", want: "agent-user_1"},
		{name: "shell metachar rejected", in: "scion;rm -rf /", want: "scion"},
		{name: "command substitution rejected", in: "$(whoami)", want: "scion"},
		{name: "quote rejected", in: `bad"name`, want: "scion"},
		{name: "space rejected", in: "two words", want: "scion"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeExecUser(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeExecUser(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
