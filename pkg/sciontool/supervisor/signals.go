/*
Copyright 2025 The Scion Authors.
*/

package supervisor

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// PreStopHook is a function called before shutdown begins.
type PreStopHook func() error

// SignalHandler handles OS signals and forwards them to the supervisor.
type SignalHandler struct {
	supervisor  *Supervisor
	sigChan     chan os.Signal
	cancel      context.CancelFunc
	preStopHook PreStopHook
}

// NewSignalHandler creates a signal handler that forwards signals to the supervisor.
func NewSignalHandler(supervisor *Supervisor, cancel context.CancelFunc) *SignalHandler {
	return &SignalHandler{
		supervisor: supervisor,
		sigChan:    make(chan os.Signal, 1),
		cancel:     cancel,
	}
}

// WithPreStopHook sets a hook function to call before shutdown begins.
func (h *SignalHandler) WithPreStopHook(hook PreStopHook) *SignalHandler {
	h.preStopHook = hook
	return h
}

// Start begins listening for signals. It handles SIGTERM and SIGINT by
// running pre-stop hooks and then cancelling the context (which triggers
// graceful shutdown), and forwards SIGHUP to the child process.
func (h *SignalHandler) Start() {
	signal.Notify(h.sigChan, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	go func() {
		for sig := range h.sigChan {
			switch sig {
			case syscall.SIGTERM, syscall.SIGINT:
				// Run pre-stop hook before triggering shutdown
				if h.preStopHook != nil {
					_ = h.preStopHook() // Best effort, don't block shutdown on errors
				}
				// Trigger graceful shutdown by cancelling context
				h.cancel()
				return
			case syscall.SIGHUP:
				// Forward SIGHUP to child (can be used for reload in future)
				_ = h.supervisor.Signal(sig)
			}
		}
	}()
}

// Stop stops the signal handler.
func (h *SignalHandler) Stop() {
	signal.Stop(h.sigChan)
	close(h.sigChan)
}
