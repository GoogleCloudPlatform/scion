/*
Copyright 2025 The Scion Authors.
*/

package supervisor

import (
	"context"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestSignalHandler_WithPreStopHook(t *testing.T) {
	// Create a supervisor and context
	sup := New(DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Track if pre-stop hook was called
	var hookCalled atomic.Bool

	// Create signal handler with pre-stop hook
	handler := NewSignalHandler(sup, cancel).
		WithPreStopHook(func() error {
			hookCalled.Store(true)
			return nil
		})

	handler.Start()
	defer handler.Stop()

	// Start a long-running command in the background
	go func() { _, _ = sup.Run(ctx, []string{"sleep", "60"}) }()
	time.Sleep(50 * time.Millisecond) // Let process start

	// Send SIGTERM
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)

	// Wait for hook to be called
	time.Sleep(100 * time.Millisecond)

	if !hookCalled.Load() {
		t.Error("pre-stop hook was not called")
	}
}

func TestSignalHandler_WithoutPreStopHook(t *testing.T) {
	// Verify handler works without pre-stop hook (nil)
	sup := New(DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := NewSignalHandler(sup, cancel)
	handler.Start()
	defer handler.Stop()

	// Start a long-running command
	go func() { _, _ = sup.Run(ctx, []string{"sleep", "60"}) }()
	time.Sleep(50 * time.Millisecond)

	// This should not panic
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	time.Sleep(100 * time.Millisecond)
}

func TestNewSignalHandler(t *testing.T) {
	sup := New(DefaultConfig())
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := NewSignalHandler(sup, cancel)

	if handler == nil {
		t.Fatal("NewSignalHandler returned nil")
	}
	if handler.supervisor != sup {
		t.Error("supervisor not set correctly")
	}
	if handler.cancel == nil {
		t.Error("cancel function not set")
	}
	if handler.sigChan == nil {
		t.Error("signal channel not initialized")
	}
	if handler.preStopHook != nil {
		t.Error("preStopHook should be nil by default")
	}
}

func TestSignalHandler_WithPreStopHook_Chaining(t *testing.T) {
	sup := New(DefaultConfig())
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	hookFunc := func() error { return nil }
	handler := NewSignalHandler(sup, cancel).WithPreStopHook(hookFunc)

	if handler.preStopHook == nil {
		t.Error("preStopHook should be set after WithPreStopHook")
	}
}
