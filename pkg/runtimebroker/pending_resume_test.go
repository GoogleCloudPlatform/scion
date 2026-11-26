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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

type recordedBrokerDelivery struct {
	text      string
	suffix    string
	interrupt bool
}

type wakeResumeFakeManager struct {
	agent.Manager
	mu          sync.Mutex
	startOpts   []api.StartOptions
	startErr    error
	deliveries  []recordedBrokerDelivery
	flushes     []string
	flushCh     chan string
	msgBuf      *agent.MessageBuffer
	deferWake   string
	deferSingle string
}

func newWakeResumeFakeManager(t *testing.T) *wakeResumeFakeManager {
	t.Helper()
	m := &wakeResumeFakeManager{
		flushCh:     make(chan string, 8),
		deferWake:   harness.WakeResumeInstruction(nil),
		deferSingle: harness.DefaultResumePrompt,
	}
	m.msgBuf = agent.NewMessageBuffer(25*time.Millisecond, func(_, _, msg string, _ bool) error {
		m.mu.Lock()
		m.flushes = append(m.flushes, msg)
		m.mu.Unlock()
		m.flushCh <- msg
		return nil
	})
	t.Cleanup(func() { m.msgBuf.Close() })
	return m
}

func (m *wakeResumeFakeManager) Provision(context.Context, api.StartOptions) (*api.ScionConfig, error) {
	return nil, nil
}

func (m *wakeResumeFakeManager) Start(_ context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.mu.Lock()
	m.startOpts = append(m.startOpts, opts)
	startErr := m.startErr
	wake := m.deferWake
	single := m.deferSingle
	m.mu.Unlock()

	if opts.WakeForMessage && opts.OnDeferredResume != nil {
		opts.OnDeferredResume(wake, single)
	}
	if startErr != nil {
		return nil, startErr
	}
	return &api.AgentInfo{
		ID:          opts.Name,
		Name:        opts.Name,
		ContainerID: "ctr-" + opts.Name,
		Phase:       "running",
	}, nil
}

func (m *wakeResumeFakeManager) Stop(context.Context, string, string, string, string) error {
	return nil
}
func (m *wakeResumeFakeManager) StopTarget(context.Context, runtime.RunRef) error { return nil }
func (m *wakeResumeFakeManager) Delete(context.Context, string, bool, string, string, bool) (bool, error) {
	return false, nil
}
func (m *wakeResumeFakeManager) DeleteTarget(context.Context, string, string, runtime.RunRef, bool, string, bool) (bool, error) {
	return false, nil
}
func (m *wakeResumeFakeManager) List(context.Context, map[string]string) ([]api.AgentInfo, error) {
	return []api.AgentInfo{{
		ID:          "agent-1",
		Name:        "agent-1",
		ContainerID: "ctr-agent-1",
		Phase:       "running",
		Labels: map[string]string{
			"scion.agent":      "true",
			"scion.name":       "agent-1",
			"scion.project_id": "proj-1",
		},
	}}, nil
}

func (m *wakeResumeFakeManager) Message(ctx context.Context, agentID, projectID, message string, interrupt bool) error {
	suffix := agent.ResumeSuffixFromContext(ctx)
	m.mu.Lock()
	m.deliveries = append(m.deliveries, recordedBrokerDelivery{
		text:      message,
		suffix:    suffix,
		interrupt: interrupt,
	})
	m.mu.Unlock()
	combined := message
	if suffix != "" {
		combined += "\n\n" + suffix
	}
	m.msgBuf.Send(agentID, projectID, combined)
	return nil
}

func (m *wakeResumeFakeManager) CleanupLaunch(context.Context, []agent.ResourceHandle) error {
	return nil
}
func (m *wakeResumeFakeManager) Close() {}

func newWakeResumeTestBroker(t *testing.T, mgr agent.Manager) *Server {
	t.Helper()
	srv := newTestServerWithManager(t, mgr)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv
}

func doBrokerJSON(t *testing.T, srv *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// TestBrokerWakeForMessage_DeliversMessageAndResumeAsOneInput (T4) verifies
// end-to-end on the broker side that:
//   - Starting with wakeForMessage:true followed by POST /message delivers the
//     waking message and wake resume instruction together in ONE paste, with
//     the message before the instruction.
//   - Starting without wakeForMessage leaves manual resume unchanged (no suffix).
func TestBrokerWakeForMessage_DeliversMessageAndResumeAsOneInput(t *testing.T) {
	mgr := newWakeResumeFakeManager(t)
	srv := newWakeResumeTestBroker(t, mgr)

	// 1. Wake start with wakeForMessage=true.
	w := doBrokerJSON(t, srv, http.MethodPost, "/api/v1/agents/agent-1/start?projectId=proj-1", map[string]any{
		"resume":         true,
		"wakeForMessage": true,
	})
	if w.Code != http.StatusOK && w.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, body = %s", w.Code, w.Body.String())
	}

	// 2. Deliver the waking message.
	const wakingMsg = "Please check the failing build."
	w = doBrokerJSON(t, srv, http.MethodPost, "/api/v1/agents/agent-1/message?projectId=proj-1", map[string]any{
		"message": wakingMsg,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("message status = %d, body = %s", w.Code, w.Body.String())
	}

	select {
	case flushed := <-mgr.flushCh:
		// Primary: delivered together as one input.
		if !strings.Contains(flushed, wakingMsg) || !strings.Contains(flushed, mgr.deferWake) {
			t.Fatalf("flush missing message or wake instruction: %q", flushed)
		}
		// Secondary: message before instruction.
		if strings.Index(flushed, wakingMsg) >= strings.Index(flushed, mgr.deferWake) {
			t.Fatalf("expected message before wake instruction in %q", flushed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for wake flush")
	}

	// Verify only one flush was emitted.
	time.Sleep(60 * time.Millisecond)
	mgr.mu.Lock()
	flushCount := len(mgr.flushes)
	mgr.mu.Unlock()
	if flushCount != 1 {
		t.Fatalf("expected exactly 1 flush on wake, got %d", flushCount)
	}

	// 3. Repeat with wakeForMessage absent (manual resume): message carries no suffix.
	w = doBrokerJSON(t, srv, http.MethodPost, "/api/v1/agents/agent-1/start?projectId=proj-1", map[string]any{
		"resume": true,
	})
	if w.Code != http.StatusOK && w.Code != http.StatusAccepted {
		t.Fatalf("manual start status = %d, body = %s", w.Code, w.Body.String())
	}
	w = doBrokerJSON(t, srv, http.MethodPost, "/api/v1/agents/agent-1/message?projectId=proj-1", map[string]any{
		"message": "ordinary follow-up",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("follow-up message status = %d, body = %s", w.Code, w.Body.String())
	}

	select {
	case flushed := <-mgr.flushCh:
		if flushed != "ordinary follow-up" {
			t.Errorf("manual resume follow-up = %q, want %q (no suffix)", flushed, "ordinary follow-up")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for manual resume follow-up flush")
	}
}

// TestPendingResume_TTLExpiryDeliversStandalone (T5) verifies that when no
// waking message arrives before TTL expiry, the standalone resume instruction
// is delivered once, and a message arriving after expiry carries no suffix.
func TestPendingResume_TTLExpiryDeliversStandalone(t *testing.T) {
	mgr := newWakeResumeFakeManager(t)
	srv := newWakeResumeTestBroker(t, mgr)
	srv.pendingResumeTTL = 40 * time.Millisecond

	w := doBrokerJSON(t, srv, http.MethodPost, "/api/v1/agents/agent-1/start?projectId=proj-1", map[string]any{
		"resume":         true,
		"wakeForMessage": true,
	})
	if w.Code != http.StatusOK && w.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, body = %s", w.Code, w.Body.String())
	}

	select {
	case flushed := <-mgr.flushCh:
		if flushed != mgr.deferSingle {
			t.Fatalf("TTL fallback delivered %q, want standalone %q", flushed, mgr.deferSingle)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TTL fallback delivery")
	}

	// A later message must carry no suffix.
	w = doBrokerJSON(t, srv, http.MethodPost, "/api/v1/agents/agent-1/message?projectId=proj-1", map[string]any{
		"message": "late message",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("late message status = %d, body = %s", w.Code, w.Body.String())
	}
	select {
	case flushed := <-mgr.flushCh:
		if flushed != "late message" {
			t.Errorf("late message = %q, want %q (no suffix after expiry)", flushed, "late message")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for late message flush")
	}
}

// TestPendingResume_TakeRacingExpiryDeliversOnce (T6) verifies under -race
// that concurrent take and TTL expiry deliver the instruction exactly once.
func TestPendingResume_TakeRacingExpiryDeliversOnce(t *testing.T) {
	reg := newPendingResumeRegistry()
	defer reg.close()

	for i := 0; i < 100; i++ {
		var delivered atomic.Int32
		key := pendingResumeKey("agent-race", "proj-1")
		reg.put(key, pendingResume{
			agentID:    "agent-race",
			projectID:  "proj-1",
			wake:       "wake-text",
			standalone: "standalone-text",
			runID:      "run-1",
		}, 2*time.Millisecond, func(pendingResume) {
			delivered.Add(1)
		})

		time.Sleep(time.Duration(i%4) * time.Millisecond)
		if _, ok := reg.take(key); ok {
			delivered.Add(1)
		}
		time.Sleep(10 * time.Millisecond)

		if got := delivered.Load(); got != 1 {
			t.Fatalf("iteration %d: delivered %d times, want exactly 1", i, got)
		}
	}
}

// TestPendingResume_DropOnLifecycleTransitions (T7) verifies that a non-wake
// start, failed start, stop, or delete drops any pending entry so its TTL
// timer never fires.
func TestPendingResume_DropOnLifecycleTransitions(t *testing.T) {
	cases := []struct {
		name   string
		action func(t *testing.T, srv *Server, mgr *wakeResumeFakeManager)
	}{
		{
			name: "non-wake start drops pending entry",
			action: func(t *testing.T, srv *Server, _ *wakeResumeFakeManager) {
				w := doBrokerJSON(t, srv, http.MethodPost, "/api/v1/agents/agent-1/start?projectId=proj-1", map[string]any{
					"resume": true,
				})
				if w.Code != http.StatusOK && w.Code != http.StatusAccepted {
					t.Fatalf("non-wake start status = %d", w.Code)
				}
			},
		},
		{
			name: "stop drops pending entry",
			action: func(t *testing.T, srv *Server, _ *wakeResumeFakeManager) {
				w := doBrokerJSON(t, srv, http.MethodPost, "/api/v1/agents/agent-1/stop?projectId=proj-1", nil)
				if w.Code != http.StatusOK && w.Code != http.StatusAccepted {
					t.Fatalf("stop status = %d, body = %s", w.Code, w.Body.String())
				}
			},
		},
		{
			name: "delete drops pending entry",
			action: func(t *testing.T, srv *Server, _ *wakeResumeFakeManager) {
				w := doBrokerJSON(t, srv, http.MethodDelete, "/api/v1/agents/agent-1?projectId=proj-1", nil)
				if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
					t.Fatalf("delete status = %d, body = %s", w.Code, w.Body.String())
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newWakeResumeFakeManager(t)
			srv := newWakeResumeTestBroker(t, mgr)
			srv.pendingResumeTTL = 80 * time.Millisecond

			w := doBrokerJSON(t, srv, http.MethodPost, "/api/v1/agents/agent-1/start?projectId=proj-1", map[string]any{
				"resume":         true,
				"wakeForMessage": true,
			})
			if w.Code != http.StatusOK && w.Code != http.StatusAccepted {
				t.Fatalf("wake start status = %d", w.Code)
			}

			tc.action(t, srv, mgr)

			select {
			case unexpected := <-mgr.flushCh:
				t.Fatalf("expected dropped pending resume not to fire, got %q", unexpected)
			case <-time.After(160 * time.Millisecond):
			}
		})
	}

	t.Run("failed wake start drops pending entry", func(t *testing.T) {
		mgr := newWakeResumeFakeManager(t)
		mgr.startErr = errors.New("container launch failed")
		srv := newWakeResumeTestBroker(t, mgr)
		srv.pendingResumeTTL = 50 * time.Millisecond

		w := doBrokerJSON(t, srv, http.MethodPost, "/api/v1/agents/agent-1/start?projectId=proj-1", map[string]any{
			"resume":         true,
			"wakeForMessage": true,
		})
		if w.Code == http.StatusOK {
			t.Fatalf("expected start failure, got 200")
		}

		select {
		case unexpected := <-mgr.flushCh:
			t.Fatalf("expected failed start to drop pending resume, got %q", unexpected)
		case <-time.After(120 * time.Millisecond):
		}
	})
}
