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

package bridge

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Notifier manages a shared PostgreSQL LISTEN connection and fans out
// notifications to per-task waiters. It is an accelerator only — all
// consumers must also have a polling fallback. See design §5.2 (D7).
//
// One Notifier is created per instance. The LISTEN connection is started
// lazily on the first Register call (not at boot), so no connection is
// held when nobody is waiting — which is the idle state on Cloud Run
// (scale-to-zero safe).
type Notifier struct {
	databaseURL string
	log         *slog.Logger

	mu      sync.Mutex
	waiters map[string][]chan struct{} // taskID -> list of wake-up channels
	started bool
	cancel  context.CancelFunc
	done    chan struct{} // closed when the listen loop exits
}

// NewNotifier creates a Notifier that will connect to the given database URL
// when the first waiter registers. The Notifier does not start a connection
// until Start is called (which Register triggers lazily).
func NewNotifier(databaseURL string, log *slog.Logger) *Notifier {
	return &Notifier{
		databaseURL: databaseURL,
		log:         log,
		waiters:     make(map[string][]chan struct{}),
	}
}

// Register returns a channel that receives a signal when events are
// appended for the given taskID. The caller must call the returned
// cleanup function when done. If the LISTEN connection is not yet started,
// Register starts it lazily.
func (n *Notifier) Register(taskID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)

	n.mu.Lock()
	n.waiters[taskID] = append(n.waiters[taskID], ch)
	needsStart := !n.started
	n.mu.Unlock()

	if needsStart {
		// Best-effort start — if it fails, everything degrades to polling.
		if err := n.Start(context.Background()); err != nil {
			n.log.Warn("notifier: failed to start LISTEN connection, degrading to polling", "error", err)
		}
	}

	cleanup := func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		waiters := n.waiters[taskID]
		for i, w := range waiters {
			if w == ch {
				n.waiters[taskID] = append(waiters[:i], waiters[i+1:]...)
				break
			}
		}
		if len(n.waiters[taskID]) == 0 {
			delete(n.waiters, taskID)
		}
	}

	return ch, cleanup
}

// Start begins listening on a dedicated connection. It is safe to call
// multiple times — subsequent calls are no-ops if the listener is already
// running. The LISTEN connection is separate from the pool; it is used
// exclusively for LISTEN and is never returned to the pool.
func (n *Notifier) Start(ctx context.Context) error {
	n.mu.Lock()
	if n.started {
		n.mu.Unlock()
		return nil
	}

	conn, err := pgx.Connect(ctx, n.databaseURL)
	if err != nil {
		n.mu.Unlock()
		return err
	}

	if _, err := conn.Exec(ctx, "LISTEN a2a_task_event"); err != nil {
		conn.Close(ctx)
		n.mu.Unlock()
		return err
	}

	listenCtx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	n.started = true
	n.done = make(chan struct{})
	n.mu.Unlock()

	go n.listenLoop(listenCtx, conn)

	n.log.Info("notifier: LISTEN connection started")
	return nil
}

// Stop closes the LISTEN connection and unblocks all waiters by sending
// a signal on each channel. After Stop, new Register calls will attempt
// to restart the connection.
func (n *Notifier) Stop() {
	n.mu.Lock()
	wasStarted := n.started
	if wasStarted {
		n.cancel()
		n.started = false
	}
	done := n.done
	// Wake all waiters so they don't block waiting for a dead connection.
	// This runs regardless of whether the LISTEN connection was started —
	// callers may have registered before the connection was established.
	for _, waiters := range n.waiters {
		for _, ch := range waiters {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
	n.mu.Unlock()

	// Wait for the listen loop to exit (only if it was started).
	if wasStarted && done != nil {
		<-done
	}
	if wasStarted {
		n.log.Info("notifier: LISTEN connection stopped")
	}
}

// listenLoop is the core loop that waits for PostgreSQL notifications and
// fans them out to registered waiters. It runs on a dedicated goroutine.
// On connection error, it logs a warning and exits — the next Register call
// will attempt reconnection. While disconnected, everything degrades to
// pure polling.
func (n *Notifier) listenLoop(ctx context.Context, conn *pgx.Conn) {
	defer close(n.done)
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn.Close(closeCtx)

		n.mu.Lock()
		n.started = false
		n.mu.Unlock()
	}()

	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// Clean shutdown — Stop() was called.
				return
			}
			n.log.Warn("notifier: LISTEN connection error, degrading to polling", "error", err)
			return
		}

		taskID := notification.Payload
		n.fanOut(taskID)
	}
}

// fanOut sends a non-blocking signal to all waiters registered for the
// given taskID.
func (n *Notifier) fanOut(taskID string) {
	n.mu.Lock()
	waiters := append([]chan struct{}(nil), n.waiters[taskID]...)
	n.mu.Unlock()

	for _, ch := range waiters {
		select {
		case ch <- struct{}{}:
		default:
			// Channel already has a pending signal — skip to avoid blocking.
		}
	}
}
