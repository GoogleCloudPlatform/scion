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

package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// httptestPostFunc builds a UsageIngestPostFunc posting JSON to srvURL with
// X-Scion-Agent-Token auth, matching pkg/sciontool/hub.Client.PostUsageEvents.
func httptestPostFunc(srvURL, agentID, token string) UsageIngestPostFunc {
	client := &http.Client{Timeout: 5 * time.Second}
	endpoint := srvURL + telemetrycontract.UsageIngestAgentPath(agentID)
	return func(ctx context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
		if req.Events == nil {
			req.Events = []telemetrycontract.UsageEvent{}
		}
		body, err := json.Marshal(req)
		if err != nil {
			return nil, err
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("X-Scion-Agent-Token", token)
		resp, err := client.Do(httpReq)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			err := fmt.Errorf("hub HTTP %d: %s", resp.StatusCode, string(respBody))
			if resp.StatusCode == http.StatusNotFound {
				return nil, fmt.Errorf("%w: %v", ErrUsageIngestRevoked, err)
			}
			if resp.StatusCode == http.StatusRequestEntityTooLarge {
				return nil, fmt.Errorf("%w: %v", ErrPayloadTooLarge, err)
			}
			if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity {
				return nil, PermanentSendError(err)
			}
			return nil, err
		}
		var out telemetrycontract.UsageIngestResponse
		if len(bytes.TrimSpace(respBody)) > 0 {
			if err := json.Unmarshal(respBody, &out); err != nil {
				return nil, err
			}
		}
		return &out, nil
	}
}

func TestHubUsageSink_BatchLimitsEventsAndBytes(t *testing.T) {
	t.Run("max batch events and ACK removes outbox entries", func(t *testing.T) {
		var mu sync.Mutex
		var batchSizes []int
		var receivedIDs []string
		var receivedSeqs []int64
		var receivedEpochs []string

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("X-Scion-Agent-Token"); got != "tok-1" {
				t.Errorf("X-Scion-Agent-Token = %q, want tok-1", got)
			}
			var req telemetrycontract.UsageIngestRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			if len(req.Events) > 0 {
				batchSizes = append(batchSizes, len(req.Events))
				for _, ev := range req.Events {
					receivedIDs = append(receivedIDs, ev.EventID)
					receivedSeqs = append(receivedSeqs, ev.ProducerSeq)
					receivedEpochs = append(receivedEpochs, ev.ProducerEpoch)
				}
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{
				Accepted: len(req.Events),
			})
		}))
		defer srv.Close()

		o := testOutbox(t)
		var wantIDs []string
		for i := 1; i <= 5; i++ {
			ev := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, fmt.Sprintf("cmp1:%d", i), int64(i*10)), AppendWritten)
			wantIDs = append(wantIDs, ev.EventID)
		}

		sink := NewHubUsageSink(o, HubUsageSinkConfig{
			Post:           httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID:          "run-1",
			HubID:          "hub-1",
			MaxBatchEvents: 2,
			MaxBatchBytes:  telemetrycontract.UsageIngestMaxBatchBytes,
		})
		d := NewHubAnalyticsDrainer(o, sink, trustedHub("hub-1"))

		sent := d.RunOnce(context.Background())
		if sent != 5 {
			t.Fatalf("sent = %d, want 5", sent)
		}

		mu.Lock()
		defer mu.Unlock()
		if len(batchSizes) != 3 || batchSizes[0] != 2 || batchSizes[1] != 2 || batchSizes[2] != 1 {
			t.Errorf("batchSizes = %v, want [2 2 1]", batchSizes)
		}
		if len(receivedIDs) != 5 {
			t.Fatalf("received %d events, want 5", len(receivedIDs))
		}
		for i := range wantIDs {
			if receivedIDs[i] != wantIDs[i] {
				t.Errorf("event[%d] ID = %q, want %q", i, receivedIDs[i], wantIDs[i])
			}
			if receivedSeqs[i] != int64(i+1) {
				t.Errorf("event[%d] producer_seq = %d, want %d", i, receivedSeqs[i], i+1)
			}
			if receivedEpochs[i] == "" || receivedEpochs[i] != receivedEpochs[0] {
				t.Errorf("event[%d] producer_epoch = %q, want non-empty uniform epoch", i, receivedEpochs[i])
			}
		}

		// ACK must remove ready files and leave PendingBytes == 0.
		ledger, err := o.LedgerSnapshot()
		if err != nil {
			t.Fatalf("LedgerSnapshot: %v", err)
		}
		if ledger.PendingBytes != 0 {
			t.Errorf("PendingBytes = %d after ACK, want 0", ledger.PendingBytes)
		}
		ready, err := o.readyFiles()
		if err != nil {
			t.Fatalf("readyFiles: %v", err)
		}
		if len(ready) != 0 {
			t.Errorf("ready files remaining after ACK = %v, want empty", ready)
		}
	})

	t.Run("max batch bytes enforced on HTTP body", func(t *testing.T) {
		const maxBytes = 1400
		var mu sync.Mutex
		var bodySizes []int
		totalEvents := 0

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var req telemetrycontract.UsageIngestRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				t.Errorf("unmarshal: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			if len(req.Events) > 0 {
				bodySizes = append(bodySizes, len(raw))
				totalEvents += len(req.Events)
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{
				Accepted: len(req.Events),
			})
		}))
		defer srv.Close()

		o := testOutbox(t)
		for i := 1; i <= 4; i++ {
			mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, fmt.Sprintf("cmp1:bytes-%d", i), int64(i)), AppendWritten)
		}

		sink := NewHubUsageSink(o, HubUsageSinkConfig{
			Post:           httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID:          "run-1",
			HubID:          "hub-1",
			MaxBatchEvents: 500,
			MaxBatchBytes:  maxBytes,
		})
		d := NewHubAnalyticsDrainer(o, sink, trustedHub("hub-1"))

		if sent := d.RunOnce(context.Background()); sent != 4 {
			t.Fatalf("sent = %d, want 4", sent)
		}

		mu.Lock()
		defer mu.Unlock()
		if totalEvents != 4 {
			t.Fatalf("totalEvents = %d, want 4", totalEvents)
		}
		if len(bodySizes) < 2 {
			t.Errorf("expected multiple batches under %d byte cap, got %d batch(es): %v", maxBytes, len(bodySizes), bodySizes)
		}
		for i, sz := range bodySizes {
			if sz > maxBytes {
				t.Errorf("batch[%d] body size = %d > maxBytes %d", i, sz, maxBytes)
			}
		}
	})
}

func TestHubUsageSink_LostACKIdempotentReplay(t *testing.T) {
	var attempts atomic.Int32
	var mu sync.Mutex
	var attemptEventIDs [][]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(req.Events) == 0 {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{})
			return
		}
		ids := make([]string, len(req.Events))
		for i, ev := range req.Events {
			ids[i] = ev.EventID
		}
		mu.Lock()
		attemptEventIDs = append(attemptEventIDs, ids)
		mu.Unlock()

		// First attempt: simulate lost ACK (503 transient failure after Hub saw the events).
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("transient"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{
			Accepted:   0,
			Duplicates: len(req.Events),
		})
	}))
	defer srv.Close()

	o := testOutbox(t)
	ev1 := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:r1", 10), AppendWritten)
	ev2 := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:r2", 20), AppendWritten)

	sink := NewHubUsageSink(o, HubUsageSinkConfig{
		Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
		RunID: "run-1",
	})
	d := NewHubAnalyticsDrainer(o, sink, trustedHub("hub-1"))
	clock := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return clock }

	// First drain: lost ACK -> 0 sent, backoff engaged, outbox still pending.
	if sent := d.RunOnce(context.Background()); sent != 0 {
		t.Fatalf("first drain sent = %d, want 0", sent)
	}
	if d.Backoff() != analyticsBackoffMin {
		t.Errorf("backoff = %v, want %v", d.Backoff(), analyticsBackoffMin)
	}
	if o.Stats().PendingBytes == 0 {
		t.Fatal("PendingBytes must remain non-zero after lost ACK")
	}

	// Within backoff: no resend yet.
	d.RunOnce(context.Background())
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts during backoff = %d, want 1", got)
	}

	// Advance past backoff: resend yields identical event IDs and clears outbox.
	clock = clock.Add(2 * time.Second)
	if sent := d.RunOnce(context.Background()); sent != 2 {
		t.Fatalf("retry drain sent = %d, want 2", sent)
	}
	if d.Backoff() != 0 {
		t.Errorf("backoff after recovery = %v, want 0", d.Backoff())
	}
	if o.Stats().PendingBytes != 0 {
		t.Errorf("PendingBytes = %d after ACK, want 0", o.Stats().PendingBytes)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(attemptEventIDs) != 2 {
		t.Fatalf("attempts recorded = %d, want 2", len(attemptEventIDs))
	}
	for i, want := range []string{ev1.EventID, ev2.EventID} {
		if attemptEventIDs[0][i] != want || attemptEventIDs[1][i] != want {
			t.Errorf("event[%d] mismatch across replay: attempt1=%q attempt2=%q want=%q",
				i, attemptEventIDs[0][i], attemptEventIDs[1][i], want)
		}
	}
}

func TestHubUsageSink_KillRestartReplayFromWAL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "outbox")

	// Process 1: append 4 events, drain batch 1 (2 events) OK, fail batch 2 (simulating kill mid-drain).
	var batchNum atomic.Int32
	var mu sync.Mutex
	var deliveredIDs []string
	var deliveredEpochs []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(req.Events) == 0 {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{})
			return
		}
		if batchNum.Add(1) == 2 {
			// Simulate network failure / kill before ACK of batch 2.
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		mu.Lock()
		for _, ev := range req.Events {
			deliveredIDs = append(deliveredIDs, ev.EventID)
			deliveredEpochs = append(deliveredEpochs, ev.ProducerEpoch)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{
			Accepted: len(req.Events),
		})
	}))
	defer srv.Close()

	o1, err := OpenAnalyticsOutbox(dir)
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(1): %v", err)
	}
	var wantIDs []string
	for i := 1; i <= 4; i++ {
		ev := mustAppend(t, o1, draftUsage(telemetrycontract.SourceIdentityComposite, fmt.Sprintf("cmp1:wal-%d", i), int64(i)), AppendWritten)
		wantIDs = append(wantIDs, ev.EventID)
	}
	ledger1, err := o1.LedgerSnapshot()
	if err != nil {
		t.Fatalf("LedgerSnapshot(1): %v", err)
	}

	sink1 := NewHubUsageSink(o1, HubUsageSinkConfig{
		Post:           httptestPostFunc(srv.URL, "agent-1", "tok-1"),
		RunID:          "run-1",
		MaxBatchEvents: 2,
	})
	d1 := NewHubAnalyticsDrainer(o1, sink1, trustedHub("hub-1"))
	if sent := d1.RunOnce(context.Background()); sent != 2 {
		t.Fatalf("process 1 sent = %d, want 2 (batch 1 ACKed, batch 2 failed)", sent)
	}
	// Process 1 "dies" without Stop.

	// Process 2: reopen same WAL directory and drain remaining events.
	o2, err := OpenAnalyticsOutbox(dir)
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(2): %v", err)
	}
	sink2 := NewHubUsageSink(o2, HubUsageSinkConfig{
		Post:           httptestPostFunc(srv.URL, "agent-1", "tok-1"),
		RunID:          "run-1",
		MaxBatchEvents: 2,
	})
	d2 := NewHubAnalyticsDrainer(o2, sink2, trustedHub("hub-1"))
	if sent := d2.RunOnce(context.Background()); sent != 2 {
		t.Fatalf("process 2 replay sent = %d, want 2", sent)
	}
	if o2.Stats().PendingBytes != 0 {
		t.Errorf("PendingBytes after replay = %d, want 0", o2.Stats().PendingBytes)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(deliveredIDs) != 4 {
		t.Fatalf("delivered = %d, want 4", len(deliveredIDs))
	}
	for i := range wantIDs {
		if deliveredIDs[i] != wantIDs[i] {
			t.Errorf("delivered[%d] = %q, want %q", i, deliveredIDs[i], wantIDs[i])
		}
		if deliveredEpochs[i] != ledger1.Epoch {
			t.Errorf("delivered[%d] epoch = %q, want persisted %q", i, deliveredEpochs[i], ledger1.Epoch)
		}
	}
}

func TestHubUsageSink_RejectedEventsDeadLetteredNotRetried(t *testing.T) {
	var mu sync.Mutex
	var batchesReceived int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(req.Events) == 0 {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{})
			return
		}
		mu.Lock()
		batchesReceived++
		mu.Unlock()
		// Reject index 1 permanently, accept indices 0 and 2.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{
			Accepted: 2,
			Rejected: []telemetrycontract.UsageIngestRejection{
				{
					Index:   1,
					EventID: req.Events[1].EventID,
					Code:    telemetrycontract.UsageRejectInvalidEvent,
					Reason:  "invalid field",
				},
			},
		})
	}))
	defer srv.Close()

	o := testOutbox(t)
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:ok-0", 1), AppendWritten)
	badEv := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:bad-1", 2), AppendWritten)
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:ok-2", 3), AppendWritten)

	sink := NewHubUsageSink(o, HubUsageSinkConfig{
		Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
		RunID: "run-1",
	})
	d := NewHubAnalyticsDrainer(o, sink, trustedHub("hub-1"))

	if sent := d.RunOnce(context.Background()); sent != 2 {
		t.Fatalf("sent = %d, want 2 (1 rejected)", sent)
	}
	// Second RunOnce must NOT retry the rejected event.
	if sent := d.RunOnce(context.Background()); sent != 0 {
		t.Fatalf("second RunOnce sent = %d, want 0", sent)
	}

	mu.Lock()
	gotBatches := batchesReceived
	mu.Unlock()
	if gotBatches != 1 {
		t.Errorf("batchesReceived = %d, want 1 (rejected event must not loop)", gotBatches)
	}

	ledger, err := o.LedgerSnapshot()
	if err != nil {
		t.Fatalf("LedgerSnapshot: %v", err)
	}
	if ledger.DeadLettersTotal != 1 {
		t.Errorf("DeadLettersTotal = %d, want 1", ledger.DeadLettersTotal)
	}
	if ledger.PendingBytes != 0 {
		t.Errorf("PendingBytes = %d, want 0", ledger.PendingBytes)
	}
	deadData, err := os.ReadFile(filepath.Join(o.Dir(), "dead_letter.jsonl"))
	if err != nil {
		t.Fatalf("read dead_letter.jsonl: %v", err)
	}
	deadLines := nonEmptyLines(deadData)
	if len(deadLines) != 1 || decodeUsage(t, deadLines[0]).EventID != badEv.EventID {
		t.Errorf("dead_letter.jsonl = %v, want single line for %q", deadLines, badEv.EventID)
	}
}

func TestHubUsageSink_HeartbeatCadenceAndFields(t *testing.T) {
	var mu sync.Mutex
	var requests []telemetrycontract.UsageIngestRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, req)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{
			Accepted: len(req.Events),
		})
	}))
	defer srv.Close()

	o := testOutbox(t)
	ledger, err := o.LedgerSnapshot()
	if err != nil {
		t.Fatalf("LedgerSnapshot: %v", err)
	}
	signals := []string{"calls", "input", "output", "cache_read", "cache_write"}
	sink := NewHubUsageSink(o, HubUsageSinkConfig{
		Post:              httptestPostFunc(srv.URL, "agent-1", "tok-1"),
		RunID:             "run-hb",
		HeartbeatInterval: 60 * time.Second,
		SupportedSignals:  func() []string { return signals },
	})
	clock := time.Date(2026, 11, 25, 14, 0, 0, 0, time.UTC)
	sink.now = func() time.Time { return clock }

	d := NewHubAnalyticsDrainer(o, sink, trustedHub("hub-1"))
	d.now = func() time.Time { return clock }

	// t=0: initial empty heartbeat sent immediately.
	d.RunOnce(context.Background())
	// t=30s: not due yet.
	clock = clock.Add(30 * time.Second)
	d.RunOnce(context.Background())
	// t=60s: second empty heartbeat due.
	clock = clock.Add(30 * time.Second)
	d.RunOnce(context.Background())

	// Append an event at t=70s -> non-empty batch advances producer_seq & last_source_activity_at.
	clock = clock.Add(10 * time.Second)
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:hb-1", 42), AppendWritten)
	d.RunOnce(context.Background())

	// t=100s (30s after batch): heartbeat timer was reset by batch -> no heartbeat yet.
	clock = clock.Add(30 * time.Second)
	d.RunOnce(context.Background())

	// t=130s (60s after batch): heartbeat due with updated producer_seq=1 and last_source_activity_at.
	clock = clock.Add(30 * time.Second)
	d.RunOnce(context.Background())

	mu.Lock()
	defer mu.Unlock()
	// Expect: [0] initial HB, [1] t=60s HB, [2] t=70s batch (1 event), [3] t=130s HB.
	if len(requests) != 4 {
		t.Fatalf("requests count = %d, want 4: %+v", len(requests), requests)
	}
	hb0 := requests[0]
	if len(hb0.Events) != 0 || hb0.ProducerEpoch != ledger.Epoch || hb0.ProducerSeq != 0 || hb0.RunID != "run-hb" {
		t.Errorf("initial HB = %+v", hb0)
	}
	if len(hb0.SupportedSignals) != len(signals) {
		t.Errorf("initial HB supported_signals = %v, want %v", hb0.SupportedSignals, signals)
	}
	hbLast := requests[3]
	if len(hbLast.Events) != 0 {
		t.Errorf("final HB events = %d, want 0", len(hbLast.Events))
	}
	if hbLast.ProducerEpoch != ledger.Epoch || hbLast.ProducerSeq != 1 {
		t.Errorf("final HB epoch/seq = %q/%d, want %q/1", hbLast.ProducerEpoch, hbLast.ProducerSeq, ledger.Epoch)
	}
	if hbLast.LastSourceActivityAt == "" {
		t.Error("final HB missing last_source_activity_at after event activity")
	}
	if len(hbLast.SupportedSignals) != len(signals) {
		t.Errorf("final HB supported_signals = %v, want %v", hbLast.SupportedSignals, signals)
	}
}

func TestHubUsageSink_ShutdownDrain(t *testing.T) {
	var failFirst atomic.Bool
	failFirst.Store(true)
	var mu sync.Mutex
	var delivered int
	var finalStates []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if failFirst.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		mu.Lock()
		delivered += len(req.Events)
		if req.FinalState != "" {
			finalStates = append(finalStates, req.FinalState)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{
			Accepted: len(req.Events),
		})
	}))
	defer srv.Close()

	o := testOutbox(t)
	_, _ = o.PinHubID("hub-1")
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:shut-1", 5), AppendWritten)

	sink := NewHubUsageSink(o, HubUsageSinkConfig{
		Post:          httptestPostFunc(srv.URL, "agent-1", "tok-1"),
		RunID:         "run-shut",
		DrainInterval: time.Hour,
	})
	d := NewHubAnalyticsDrainer(o, sink, trustedHub("hub-1"))

	// Trigger backoff via failed RunOnce.
	d.RunOnce(context.Background())
	if d.Backoff() == 0 {
		t.Fatal("expected backoff after transient failure")
	}

	// Recover Hub and invoke Stop: must ignore backoff, drain immediately, and send final_state="final".
	failFirst.Store(false)
	d.Start(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.Stop(ctx)

	mu.Lock()
	defer mu.Unlock()
	if delivered != 1 {
		t.Errorf("shutdown drain delivered = %d, want 1", delivered)
	}
	if len(finalStates) != 1 || finalStates[0] != telemetrycontract.MeterFinalStateFinal {
		t.Errorf("finalStates = %v, want [%q]", finalStates, telemetrycontract.MeterFinalStateFinal)
	}
	if o.Stats().PendingBytes != 0 {
		t.Errorf("PendingBytes after shutdown drain = %d, want 0", o.Stats().PendingBytes)
	}
}

// O1: a failed final drain (Hub 5xx at shutdown) or remaining pending bytes at
// shutdown must send final_state="incomplete" rather than "final".
func TestHubUsageSink_ShutdownDrainIncomplete(t *testing.T) {
	t.Run("failed final drain (Hub 5xx) sends final_state=incomplete", func(t *testing.T) {
		var mu sync.Mutex
		var finalStates []string

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req telemetrycontract.UsageIngestRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// Fail event batches with 503 during shutdown drain, but accept the
			// final heartbeat so we can observe its final_state.
			if len(req.Events) > 0 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("hub overloaded"))
				return
			}
			mu.Lock()
			if req.FinalState != "" {
				finalStates = append(finalStates, req.FinalState)
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{})
		}))
		defer srv.Close()

		o := testOutbox(t)
		mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:shut-inc-5xx", 1), AppendWritten)

		sink := NewHubUsageSink(o, HubUsageSinkConfig{
			Post:          httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID:         "run-shut-5xx",
			DrainInterval: time.Hour,
		})
		d := NewHubAnalyticsDrainer(o, sink, trustedHub("hub-1"))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		d.Stop(ctx)

		mu.Lock()
		defer mu.Unlock()
		if len(finalStates) != 1 || finalStates[0] != telemetrycontract.MeterFinalStateIncomplete {
			t.Fatalf("finalStates = %v, want [%q]", finalStates, telemetrycontract.MeterFinalStateIncomplete)
		}
		if o.Stats().PendingBytes == 0 {
			t.Error("expected PendingBytes > 0 when final drain failed")
		}
	})

	t.Run("pending bytes remaining at shutdown sends final_state=incomplete", func(t *testing.T) {
		var mu sync.Mutex
		var finalStates []string

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req telemetrycontract.UsageIngestRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			if req.FinalState != "" {
				finalStates = append(finalStates, req.FinalState)
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
		}))
		defer srv.Close()

		// With a nil hub resolver, provisional staging lines cannot be
		// finalized, so DrainBatch succeeds with 0 sent while PendingBytes > 0.
		o := testOutbox(t)
		mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:shut-inc-pending", 1), AppendWritten)

		sink := NewHubUsageSink(o, HubUsageSinkConfig{
			Post:          httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID:         "run-shut-pending",
			DrainInterval: time.Hour,
		})
		d := NewHubAnalyticsDrainer(o, sink, nil)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		d.Stop(ctx)

		mu.Lock()
		defer mu.Unlock()
		if len(finalStates) != 1 || finalStates[0] != telemetrycontract.MeterFinalStateIncomplete {
			t.Fatalf("finalStates = %v, want [%q]", finalStates, telemetrycontract.MeterFinalStateIncomplete)
		}
		if o.Stats().PendingBytes == 0 {
			t.Error("expected PendingBytes > 0 for unfinalized staging line")
		}
	})
}

// O3: in Hub-only mode, a 404 capability revocation detaches the native
// producer so subsequent native records are no-ops and do not grow
// analytics-outbox-hub or trip overflow counters.
func TestPipeline_HubOnlyRevocation404DetachesNativeProducer(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_USAGE_SOURCE", "native")
	t.Setenv("SCION_LAUNCH_ID", "run-hubonly-revoke")

	var hubCalls atomic.Int32
	revoked := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil && len(req.Events) == 0 {
			// Accept any startup heartbeat that races ahead of Record 1 so
			// revocation is triggered deterministically by the event-1 drain POST.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{})
			return
		}
		hubCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("revoked"))
		select {
		case revoked <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()

	cfg := &Config{
		Enabled:      true,
		CloudEnabled: false, // Hub-only mode (no GCP exporter)
		GRPCPort:     0,
		HTTPPort:     0,
	}
	p := NewWithConfig(cfg)
	agentHome := t.TempDir()
	p.SetAnalyticsOutbox(agentHome, 0, 0)
	p.SetHubUsageSink(HubUsageSinkConfig{
		Post:              httptestPostFunc(srv.URL, "agent-1", "tok-1"),
		RunID:             "run-hubonly-revoke",
		HubID:             "hub-1",
		DrainInterval:     20 * time.Millisecond,
		HeartbeatInterval: time.Hour,
	})

	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Pipeline.Start: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Stop(stopCtx)
	}()

	makeObs := func(id string) UsageObservation {
		return UsageObservation{
			SourceKind:      "claude.api_request",
			Identity:        telemetrycontract.SourceIdentityComposite,
			SourceEventID:   id,
			Status:          telemetrycontract.StatusSuccess,
			Calls:           1,
			Input:           int64Ptr(10),
			InputDerivation: telemetrycontract.InputDerivationReported,
			TokenStatus:     telemetrycontract.TokenStatusPartial,
		}
	}

	// Record event 1 to trigger a drain POST, which receives HTTP 404.
	if out, err := p.analytics.native.Record(makeObs("cmp1:ho-rev-1")); err != nil || out != AppendWritten {
		t.Fatalf("Record 1 = (%v, %v), want (AppendWritten, nil)", out, err)
	}

	// Wait until the drainer processes the 404 and marks Hub revoked.
	deadline := time.Now().Add(3 * time.Second)
	for !p.analytics.drainer.HubRevoked() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for HubRevoked() == true after 404")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if IsHubOutboxActive(agentHome) {
		t.Error("expected Hub outbox active marker to be removed after 404 revocation")
	}

	ledgerBefore, memBefore, ok, err := p.HubAnalyticsLedger()
	if !ok || err != nil {
		t.Fatalf("HubAnalyticsLedger before = ok:%v err:%v", ok, err)
	}
	if !p.analytics.native.disabled.Load() {
		t.Fatal("expected native producer to be detached before HubRevoked() became visible")
	}

	// Subsequent native records must be no-ops: no new seqs, no appends, no overflow, no errors.
	for i := 2; i <= 50; i++ {
		if out, err := p.analytics.native.Record(makeObs(fmt.Sprintf("cmp1:ho-rev-%d", i))); err != nil || out != AppendDisabled {
			t.Fatalf("Record %d after revocation = (%v, %v), want (AppendDisabled, nil)", i, out, err)
		}
	}

	ledgerAfter, memAfter, ok, err := p.HubAnalyticsLedger()
	if !ok || err != nil {
		t.Fatalf("HubAnalyticsLedger after = ok:%v err:%v", ok, err)
	}
	if ledgerAfter.LastSeq != ledgerBefore.LastSeq {
		t.Errorf("Hub outbox LastSeq advanced after 404 revocation: %d -> %d (native producer was not detached)", ledgerBefore.LastSeq, ledgerAfter.LastSeq)
	}
	if memAfter.Appended != memBefore.Appended {
		t.Errorf("Hub outbox Appended advanced after 404 revocation: %d -> %d", memBefore.Appended, memAfter.Appended)
	}
	if memAfter.Overflow != 0 || ledgerAfter.OverflowTotal != 0 {
		t.Errorf("overflow = %d (persisted %d), want 0 after revocation", memAfter.Overflow, ledgerAfter.OverflowTotal)
	}
}

func TestPipeline_HubSinkEnablesOutboxWithoutGCPExporter(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_USAGE_SOURCE", "native")

	var mu sync.Mutex
	var gotRequests []telemetrycontract.UsageIngestRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		gotRequests = append(gotRequests, req)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{
			Accepted: len(req.Events),
		})
	}))
	defer srv.Close()

	cfg := &Config{
		Enabled:      true,
		CloudEnabled: false, // No GCP exporter!
		GRPCPort:     0,
		HTTPPort:     0,
	}
	p := NewWithConfig(cfg)
	agentHome := t.TempDir()
	p.SetAnalyticsOutbox(agentHome, 0, 0)
	p.SetHubUsageSink(HubUsageSinkConfig{
		Post:              httptestPostFunc(srv.URL, "agent-1", "tok-1"),
		RunID:             "run-nogcp",
		HubID:             "hub-1",
		DrainInterval:     50 * time.Millisecond,
		HeartbeatInterval: time.Hour,
	})

	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Pipeline.Start: %v", err)
	}
	if _, ok, reason := p.AnalyticsDiagnostics(); !ok || reason != "" {
		t.Fatalf("AnalyticsDiagnostics() = ok:%v reason:%q, want ok:true reason:\"\" without GCP exporter", ok, reason)
	}
	if _, err := os.Stat(AnalyticsOutboxDirFor(agentHome)); !os.IsNotExist(err) {
		t.Errorf("Hub-only mode must not create GCP outbox dir %q", AnalyticsOutboxDirFor(agentHome))
	}
	if _, err := os.Stat(HubAnalyticsOutboxDirFor(agentHome)); err != nil {
		t.Errorf("Hub-only mode must create Hub outbox dir %q: %v", HubAnalyticsOutboxDirFor(agentHome), err)
	}
	if !IsHubOutboxActive(agentHome) {
		t.Errorf("Hub-only mode must mark Hub outbox active on disk")
	}

	// Verify hook helper also enables when EnvUsageHubIngest=1 without GCP exporter.
	t.Setenv(EnvUsageHubIngest, "")
	if AnalyticsEnabledForHooks(cfg) {
		t.Error("AnalyticsEnabledForHooks should be false when SCION_USAGE_HUB_INGEST is unset and no GCP exporter")
	}
	t.Setenv(EnvUsageHubIngest, "1")
	if !AnalyticsEnabledForHooks(cfg) {
		t.Error("AnalyticsEnabledForHooks should be true when SCION_USAGE_HUB_INGEST=1 even without GCP exporter")
	}

	// Record an event via the pipeline's native analytics producer and stop to force shutdown drain.
	if _, err := p.analytics.native.Record(UsageObservation{
		SourceKind:      "claude.api_request",
		Identity:        telemetrycontract.SourceIdentityComposite,
		SourceEventID:   "cmp1:nogcp-1",
		Status:          telemetrycontract.StatusSuccess,
		Calls:           1,
		Input:           int64Ptr(100),
		InputDerivation: telemetrycontract.InputDerivationReported,
		TokenStatus:     telemetrycontract.TokenStatusPartial,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Pipeline.Stop: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	totalEvents := 0
	sawFinal := false
	for _, r := range gotRequests {
		totalEvents += len(r.Events)
		if r.FinalState == telemetrycontract.MeterFinalStateFinal {
			sawFinal = true
		}
	}
	if totalEvents != 1 {
		t.Errorf("totalEvents drained to Hub = %d, want 1", totalEvents)
	}
	if !sawFinal {
		t.Errorf("expected shutdown final_state=%q in requests: %+v", telemetrycontract.MeterFinalStateFinal, gotRequests)
	}
}

func TestHubUsageSink_PermanentHTTPErrorDeadLetters(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("malformed"))
	}))
	defer srv.Close()

	o := testOutbox(t)
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:perm-1", 1), AppendWritten)

	sink := NewHubUsageSink(o, HubUsageSinkConfig{
		Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
		RunID: "run-perm",
	})
	d := NewHubAnalyticsDrainer(o, sink, trustedHub("hub-1"))

	d.RunOnce(context.Background())
	// Permanent 400 dead-letters the batch and does not retry it on subsequent ticks.
	if o.Stats().PendingBytes != 0 {
		t.Errorf("PendingBytes = %d, want 0 after permanent error dead-letter", o.Stats().PendingBytes)
	}
	ledger, err := o.LedgerSnapshot()
	if err != nil {
		t.Fatalf("LedgerSnapshot: %v", err)
	}
	if ledger.DeadLettersTotal != 1 {
		t.Errorf("DeadLettersTotal = %d, want 1", ledger.DeadLettersTotal)
	}
}

// R1 (scenario d): rotating producer epoch on run change ensures a new run on
// a persisted agent home starts its sequence at 1 (no [1..N] gap from the
// previous run's sequences).
func TestHubUsageSink_RunChangeRotatesEpochAndResetsSeq(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "outbox")
	o1, err := OpenAnalyticsOutbox(dir)
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(1): %v", err)
	}

	var runAEpoch string
	for i := 1; i <= 3; i++ {
		ev := draftUsage(telemetrycontract.SourceIdentityComposite, fmt.Sprintf("cmp1:runA-%d", i), int64(i))
		ev.RunID = "run-A"
		mustAppend(t, o1, ev, AppendWritten)
		if runAEpoch == "" {
			runAEpoch = ev.ProducerEpoch
		}
		if ev.ProducerSeq != int64(i) {
			t.Fatalf("run-A event %d seq = %d, want %d", i, ev.ProducerSeq, i)
		}
	}

	// Reopen persisted outbox under a new run (run-B).
	var mu sync.Mutex
	var reqs []telemetrycontract.UsageIngestRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		reqs = append(reqs, req)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
	}))
	defer srv.Close()

	o2, err := OpenAnalyticsOutbox(dir)
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(2): %v", err)
	}
	sink2 := NewHubUsageSink(o2, HubUsageSinkConfig{
		Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
		RunID: "run-B",
	})

	ledger2, err := o2.LedgerSnapshot()
	if err != nil {
		t.Fatalf("LedgerSnapshot(2): %v", err)
	}
	if ledger2.Epoch == runAEpoch {
		t.Fatalf("epoch did not rotate on run change: still %q", ledger2.Epoch)
	}
	if ledger2.LastSeq != 0 {
		t.Fatalf("seq did not reset on run change: got %d, want 0", ledger2.LastSeq)
	}

	for i := 1; i <= 2; i++ {
		ev := draftUsage(telemetrycontract.SourceIdentityComposite, fmt.Sprintf("cmp1:runB-%d", i), int64(i))
		ev.RunID = "run-B"
		mustAppend(t, o2, ev, AppendWritten)
		if ev.ProducerEpoch != ledger2.Epoch {
			t.Errorf("run-B event %d epoch = %q, want %q", i, ev.ProducerEpoch, ledger2.Epoch)
		}
		if ev.ProducerSeq != int64(i) {
			t.Errorf("run-B event %d seq = %d, want %d (must start at 1)", i, ev.ProducerSeq, i)
		}
	}

	d2 := NewHubAnalyticsDrainer(o2, sink2, trustedHub("hub-1"))
	if sent := d2.RunOnce(context.Background()); sent != 5 {
		t.Fatalf("sent = %d, want 5", sent)
	}

	mu.Lock()
	defer mu.Unlock()
	var sawRunB bool
	for _, req := range reqs {
		if req.RunID == "run-B" && len(req.Events) > 0 {
			sawRunB = true
			if req.ProducerEpoch != ledger2.Epoch {
				t.Errorf("run-B request epoch = %q, want %q", req.ProducerEpoch, ledger2.Epoch)
			}
			if req.Events[0].ProducerSeq != 1 || req.Events[1].ProducerSeq != 2 {
				t.Errorf("run-B event seqs = [%d, %d], want [1, 2]", req.Events[0].ProducerSeq, req.Events[1].ProducerSeq)
			}
		}
	}
	if !sawRunB {
		t.Fatalf("did not see run-B batch in requests: %+v", reqs)
	}
}

// R2: events appended under run A carry run_id=run-A; when replayed from the
// WAL by a restarted process configured for run B, the Hub request carries
// run-A for those events and run-B for new events.
func TestHubUsageSink_CrossRunWALReplayPreservesOriginalRunID(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_HARNESS", "claude")

	dir := filepath.Join(t.TempDir(), "outbox")
	o1, err := OpenAnalyticsOutbox(dir)
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(1): %v", err)
	}

	// Process 1 runs under SCION_LAUNCH_ID=run-A and crashes before draining.
	t.Setenv("SCION_LAUNCH_ID", "run-A")
	ua1 := NewUsageAnalytics(o1, telemetrycontract.UsageSourceNative)
	for i := 1; i <= 2; i++ {
		if _, err := ua1.Record(UsageObservation{
			SourceKind:      "claude.api_request",
			Identity:        telemetrycontract.SourceIdentityComposite,
			SourceEventID:   fmt.Sprintf("cmp1:cross-A-%d", i),
			Status:          telemetrycontract.StatusSuccess,
			Calls:           1,
			Input:           int64Ptr(int64(i * 10)),
			InputDerivation: telemetrycontract.InputDerivationReported,
			TokenStatus:     telemetrycontract.TokenStatusPartial,
		}); err != nil {
			t.Fatalf("Record(run-A, %d): %v", i, err)
		}
	}

	var mu sync.Mutex
	var reqs []telemetrycontract.UsageIngestRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		reqs = append(reqs, req)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
	}))
	defer srv.Close()

	// Process 2 restarts under run-B, appends one run-B event, and drains.
	t.Setenv("SCION_LAUNCH_ID", "run-B")
	o2, err := OpenAnalyticsOutbox(dir)
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(2): %v", err)
	}
	sink2 := NewHubUsageSink(o2, HubUsageSinkConfig{
		Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
		RunID: "run-B",
	})
	ua2 := NewUsageAnalytics(o2, telemetrycontract.UsageSourceNative)
	if _, err := ua2.Record(UsageObservation{
		SourceKind:      "claude.api_request",
		Identity:        telemetrycontract.SourceIdentityComposite,
		SourceEventID:   "cmp1:cross-B-1",
		Status:          telemetrycontract.StatusSuccess,
		Calls:           1,
		Input:           int64Ptr(99),
		InputDerivation: telemetrycontract.InputDerivationReported,
		TokenStatus:     telemetrycontract.TokenStatusPartial,
	}); err != nil {
		t.Fatalf("Record(run-B): %v", err)
	}

	d2 := NewHubAnalyticsDrainer(o2, sink2, trustedHub("hub-1"))
	if sent := d2.RunOnce(context.Background()); sent != 3 {
		t.Fatalf("sent = %d, want 3", sent)
	}

	mu.Lock()
	defer mu.Unlock()
	var runACount, runBCount int
	for _, req := range reqs {
		if len(req.Events) == 0 {
			continue
		}
		switch req.RunID {
		case "run-A":
			for _, ev := range req.Events {
				if ev.RunID != "run-A" {
					t.Errorf("run-A batch carried event with RunID=%q", ev.RunID)
				}
				runACount++
			}
		case "run-B":
			for _, ev := range req.Events {
				if ev.RunID != "run-B" {
					t.Errorf("run-B batch carried event with RunID=%q", ev.RunID)
				}
				runBCount++
			}
		default:
			t.Errorf("unexpected req.RunID = %q", req.RunID)
		}
	}
	if runACount != 2 || runBCount != 1 {
		t.Errorf("got runACount=%d runBCount=%d, want 2 and 1", runACount, runBCount)
	}
}

// Dual-run with independent per-sink outboxes: both GCP Cloud Logging and Hub
// sink receive every event via appender teeing, and failure/rejection in one
// sink never blocks, overflows, or dead-letters the other.
func TestDualAnalyticsDrainer_FanOutAndIsolation(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_LAUNCH_ID", "run-dual")

	makeObs := func(id string) UsageObservation {
		return UsageObservation{
			SourceKind:      "claude.api_request",
			Identity:        telemetrycontract.SourceIdentityComposite,
			SourceEventID:   id,
			Status:          telemetrycontract.StatusSuccess,
			Calls:           1,
			Input:           int64Ptr(10),
			Output:          int64Ptr(5),
			InputDerivation: telemetrycontract.InputDerivationReported,
			TokenStatus:     telemetrycontract.TokenStatusPartial,
		}
	}

	t.Run("both sinks receive every event via tee", func(t *testing.T) {
		var mu sync.Mutex
		var hubIDs []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req telemetrycontract.UsageIngestRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			for _, ev := range req.Events {
				hubIDs = append(hubIDs, ev.EventID)
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
		}))
		defer srv.Close()

		gcpOutbox := testOutbox(t)
		hubOutbox := testOutbox(t)
		dual := NewDualUsageAnalytics(gcpOutbox, hubOutbox, telemetrycontract.UsageSourceNative)
		for i := 1; i <= 3; i++ {
			if out, err := dual.Record(makeObs(fmt.Sprintf("cmp1:dual-%d", i))); err != nil || out != AppendWritten {
				t.Fatalf("Record[%d] = (%v, %v), want (AppendWritten, nil)", i, out, err)
			}
		}

		gcp := &capture{}
		sink := NewHubUsageSink(hubOutbox, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-dual",
		})
		d := NewDualAnalyticsDrainer(gcpOutbox, gcp.send, sink, trustedHub("hub-1"))

		d.RunOnce(context.Background())

		if len(gcp.lines) != 3 {
			t.Fatalf("gcp received %d lines, want 3", len(gcp.lines))
		}
		mu.Lock()
		gotHub := append([]string(nil), hubIDs...)
		mu.Unlock()
		if len(gotHub) != 3 {
			t.Fatalf("hub received %d events, want 3", len(gotHub))
		}
		for i := range gcp.lines {
			gcpID := decodeUsage(t, gcp.lines[i]).EventID
			if gotHub[i] != gcpID {
				t.Errorf("hub[%d] = %q, want %q (matching GCP event_id)", i, gotHub[i], gcpID)
			}
		}
		if gcpOutbox.Stats().PendingBytes != 0 {
			t.Errorf("gcp PendingBytes = %d, want 0", gcpOutbox.Stats().PendingBytes)
		}
		if hubOutbox.Stats().PendingBytes != 0 {
			t.Errorf("hub PendingBytes = %d, want 0", hubOutbox.Stats().PendingBytes)
		}
		if gcpOutbox.Stats().Delivered != 3 {
			t.Errorf("gcp Delivered = %d, want 3 (no double counting)", gcpOutbox.Stats().Delivered)
		}
		if hubOutbox.Stats().Delivered != 3 {
			t.Errorf("hub Delivered = %d, want 3", hubOutbox.Stats().Delivered)
		}
	})

	t.Run("GCP failure does not block Hub and GCP recovers independently", func(t *testing.T) {
		var mu sync.Mutex
		var hubIDs []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req telemetrycontract.UsageIngestRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			for _, ev := range req.Events {
				hubIDs = append(hubIDs, ev.EventID)
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
		}))
		defer srv.Close()

		gcpOutbox := testOutbox(t)
		hubOutbox := testOutbox(t)
		dual := NewDualUsageAnalytics(gcpOutbox, hubOutbox, telemetrycontract.UsageSourceNative)
		if _, err := dual.Record(makeObs("cmp1:gcp-fail-1")); err != nil {
			t.Fatalf("Record 1: %v", err)
		}
		if _, err := dual.Record(makeObs("cmp1:gcp-fail-2")); err != nil {
			t.Fatalf("Record 2: %v", err)
		}

		gcp := &capture{fail: errors.New("cloud logging down")}
		sink := NewHubUsageSink(hubOutbox, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-dual-2",
		})
		d := NewDualAnalyticsDrainer(gcpOutbox, gcp.send, sink, trustedHub("hub-1"))
		clock := time.Date(2026, 11, 25, 19, 0, 0, 0, time.UTC)
		d.now = func() time.Time { return clock }

		// First tick: GCP fails, Hub still delivers both events and drains its own outbox to 0.
		d.RunOnce(context.Background())
		mu.Lock()
		gotHub := len(hubIDs)
		mu.Unlock()
		if gotHub != 2 {
			t.Fatalf("hub delivered %d events while GCP failed, want 2", gotHub)
		}
		if len(gcp.lines) != 0 {
			t.Fatalf("gcp delivered %d events while down, want 0", len(gcp.lines))
		}
		if gcpOutbox.Stats().PendingBytes == 0 {
			t.Fatal("gcp PendingBytes must remain non-zero while GCP is failing")
		}
		if hubOutbox.Stats().PendingBytes != 0 {
			t.Errorf("hub PendingBytes = %d, want 0 (independent of GCP)", hubOutbox.Stats().PendingBytes)
		}

		// Recover GCP and advance clock past GCP backoff: GCP delivers both events without re-sending to Hub.
		gcp.fail = nil
		clock = clock.Add(5 * time.Second)
		d.RunOnce(context.Background())
		if len(gcp.lines) != 2 {
			t.Fatalf("gcp delivered %d events after recovery, want 2", len(gcp.lines))
		}
		mu.Lock()
		gotHubAfter := len(hubIDs)
		mu.Unlock()
		if gotHubAfter != 2 {
			t.Errorf("hub received %d events total, want 2 (no duplicate delivery)", gotHubAfter)
		}
		if gcpOutbox.Stats().PendingBytes != 0 {
			t.Errorf("gcp PendingBytes = %d after recovery, want 0", gcpOutbox.Stats().PendingBytes)
		}
	})

	t.Run("Hub failure and rejection do not block or dead-letter GCP", func(t *testing.T) {
		var hubFail atomic.Bool
		hubFail.Store(true)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req telemetrycontract.UsageIngestRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if hubFail.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			var rej []telemetrycontract.UsageIngestRejection
			if len(req.Events) > 0 {
				rej = append(rej, telemetrycontract.UsageIngestRejection{
					Index:   0,
					EventID: req.Events[0].EventID,
					Code:    telemetrycontract.UsageRejectInvalidEvent,
					Reason:  "hub rejected",
				})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{
				Accepted: len(req.Events) - len(rej),
				Rejected: rej,
			})
		}))
		defer srv.Close()

		gcpOutbox := testOutbox(t)
		hubOutbox := testOutbox(t)
		dual := NewDualUsageAnalytics(gcpOutbox, hubOutbox, telemetrycontract.UsageSourceNative)
		if _, err := dual.Record(makeObs("cmp1:hub-fail-1")); err != nil {
			t.Fatalf("Record 1: %v", err)
		}
		if _, err := dual.Record(makeObs("cmp1:hub-fail-2")); err != nil {
			t.Fatalf("Record 2: %v", err)
		}

		gcp := &capture{}
		sink := NewHubUsageSink(hubOutbox, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-dual-3",
		})
		d := NewDualAnalyticsDrainer(gcpOutbox, gcp.send, sink, trustedHub("hub-1"))
		clock := time.Date(2026, 11, 25, 19, 10, 0, 0, time.UTC)
		d.now = func() time.Time { return clock }

		// First tick: Hub is 503, GCP still receives both events cleanly and its PendingBytes is 0.
		d.RunOnce(context.Background())
		if len(gcp.lines) != 2 {
			t.Fatalf("gcp delivered %d events while Hub was down, want 2", len(gcp.lines))
		}
		if gcpOutbox.Stats().PendingBytes != 0 {
			t.Errorf("gcp PendingBytes = %d, want 0 (independent of Hub)", gcpOutbox.Stats().PendingBytes)
		}
		if hubOutbox.Stats().PendingBytes == 0 {
			t.Fatal("hub PendingBytes must remain non-zero while Hub is failing")
		}

		// Hub recovers and rejects event 0: Hub dead-letters only in hubOutbox; GCP dead letters stay 0.
		hubFail.Store(false)
		clock = clock.Add(5 * time.Second)
		d.RunOnce(context.Background())
		if len(gcp.lines) != 2 {
			t.Errorf("gcp lines = %d, want 2", len(gcp.lines))
		}
		if gcpOutbox.Stats().DeadLetters != 0 || gcpOutbox.Stats().PersistedDeadLetters != 0 {
			t.Errorf("gcp DeadLetters = %d (persisted %d), want 0", gcpOutbox.Stats().DeadLetters, gcpOutbox.Stats().PersistedDeadLetters)
		}
		if hubOutbox.Stats().DeadLetters != 1 || hubOutbox.Stats().PersistedDeadLetters != 1 {
			t.Errorf("hub DeadLetters = %d (persisted %d), want 1", hubOutbox.Stats().DeadLetters, hubOutbox.Stats().PersistedDeadLetters)
		}
		if hubOutbox.Stats().Delivered != 1 {
			t.Errorf("hub Delivered = %d, want 1", hubOutbox.Stats().Delivered)
		}
		if hubOutbox.Stats().PendingBytes != 0 {
			t.Errorf("hub PendingBytes = %d after Hub caught up, want 0", hubOutbox.Stats().PendingBytes)
		}
	})
}

// O2: HTTP 413 Payload Too Large must bisect multi-event batches instead of
// dead-lettering good events; only a single event that exceeds the limit on
// its own may be dead-lettered. Re-encoded JSON expansion is also proactively
// bisected.
func TestHubUsageSink_413BisectsBatchAndOnlyDeadLettersSingleOversizeEvent(t *testing.T) {
	t.Run("reactive 413 bisects multi-event batch without dead-lettering", func(t *testing.T) {
		var mu sync.Mutex
		var deliveredIDs []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req telemetrycontract.UsageIngestRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(req.Events) > 1 {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				_, _ = w.Write([]byte("batch too large"))
				return
			}
			mu.Lock()
			for _, ev := range req.Events {
				deliveredIDs = append(deliveredIDs, ev.EventID)
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
		}))
		defer srv.Close()

		o := testOutbox(t)
		var wantIDs []string
		for i := 1; i <= 4; i++ {
			ev := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, fmt.Sprintf("cmp1:413-%d", i), int64(i)), AppendWritten)
			wantIDs = append(wantIDs, ev.EventID)
		}

		sink := NewHubUsageSink(o, HubUsageSinkConfig{
			Post:           httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID:          "run-413",
			MaxBatchEvents: 500,
			MaxBatchBytes:  telemetrycontract.UsageIngestMaxBatchBytes,
		})
		d := NewHubAnalyticsDrainer(o, sink, trustedHub("hub-1"))

		if sent := d.RunOnce(context.Background()); sent != 4 {
			t.Fatalf("sent = %d, want 4", sent)
		}
		ledger, err := o.LedgerSnapshot()
		if err != nil {
			t.Fatalf("LedgerSnapshot: %v", err)
		}
		if ledger.DeadLettersTotal != 0 {
			t.Errorf("DeadLettersTotal = %d, want 0 (no good event may be dead-lettered on 413)", ledger.DeadLettersTotal)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(deliveredIDs) != 4 {
			t.Fatalf("delivered = %d, want 4", len(deliveredIDs))
		}
		for i, want := range wantIDs {
			if deliveredIDs[i] != want {
				t.Errorf("delivered[%d] = %q, want %q", i, deliveredIDs[i], want)
			}
		}
	})

	t.Run("single oversize event dead-lettered while good siblings succeed", func(t *testing.T) {
		const serverLimit = 1200
		var mu sync.Mutex
		var deliveredIDs []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(raw) > serverLimit {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				_, _ = w.Write([]byte("too large"))
				return
			}
			var req telemetrycontract.UsageIngestRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			for _, ev := range req.Events {
				deliveredIDs = append(deliveredIDs, ev.EventID)
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
		}))
		defer srv.Close()

		o := testOutbox(t)
		good0 := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:413-good-0", 1), AppendWritten)
		huge := draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:413-huge-1", 2)
		huge.SessionID = strings.Repeat("x", 1500) // exceeds serverLimit even on its own
		mustAppend(t, o, huge, AppendWritten)
		good2 := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:413-good-2", 3), AppendWritten)

		sink := NewHubUsageSink(o, HubUsageSinkConfig{
			Post:           httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID:          "run-413-single",
			MaxBatchEvents: 500,
			MaxBatchBytes:  telemetrycontract.UsageIngestMaxBatchBytes,
		})
		d := NewHubAnalyticsDrainer(o, sink, trustedHub("hub-1"))

		if sent := d.RunOnce(context.Background()); sent != 2 {
			t.Fatalf("sent = %d, want 2 good events", sent)
		}
		ledger, err := o.LedgerSnapshot()
		if err != nil {
			t.Fatalf("LedgerSnapshot: %v", err)
		}
		if ledger.DeadLettersTotal != 1 {
			t.Errorf("DeadLettersTotal = %d, want 1 (only the single oversize event)", ledger.DeadLettersTotal)
		}
		if ledger.PendingBytes != 0 {
			t.Errorf("PendingBytes = %d, want 0", ledger.PendingBytes)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(deliveredIDs) != 2 || deliveredIDs[0] != good0.EventID || deliveredIDs[1] != good2.EventID {
			t.Errorf("deliveredIDs = %v, want [%q, %q]", deliveredIDs, good0.EventID, good2.EventID)
		}
	})

	t.Run("proactive re-encoded JSON expansion bisects before POST without triggering 413", func(t *testing.T) {
		const maxBytes = 2000
		var mu sync.Mutex
		var maxSeenBody int
		var delivered int
		var status413Count int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			if len(raw) > maxSeenBody {
				maxSeenBody = len(raw)
			}
			if len(raw) > maxBytes {
				status413Count++
				mu.Unlock()
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				_, _ = w.Write([]byte("payload too large"))
				return
			}
			mu.Unlock()
			var req telemetrycontract.UsageIngestRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			delivered += len(req.Events)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
		}))
		defer srv.Close()

		o := testOutbox(t)
		sink := NewHubUsageSink(o, HubUsageSinkConfig{
			Post:           httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID:          "run-esc",
			MaxBatchEvents: 500,
			MaxBatchBytes:  maxBytes,
		})

		// Build two valid UsageEvent JSON lines whose raw unescaped bytes fit
		// comfortably within EffectiveMaxBatchBytes (so a single 2-event batch
		// enters SendBatch), but whose '<' characters expand 1->6 bytes
		// (\u003c) when encoding/json re-marshals UsageIngestRequest, pushing
		// the combined request body over maxBytes.
		unescapedLine := func(id string, seq int64) []byte {
			ev := draftUsage(telemetrycontract.SourceIdentityComposite, id, seq)
			ev.EventID = id
			ev.HubID = "hub-1"
			ev.RunID = "run-esc"
			ev.ProducerEpoch = "ep-1"
			ev.ProducerSeq = seq
			ev.SourceOccurrence = 1
			ev.SessionID = strings.Repeat("<", 100)
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(ev); err != nil {
				t.Fatalf("encode unescaped line: %v", err)
			}
			return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
		}
		line1 := unescapedLine("u1-proactive-1", 1)
		line2 := unescapedLine("u1-proactive-2", 2)
		if eff := sink.EffectiveMaxBatchBytes(); len(line1)+1+len(line2) > eff {
			t.Fatalf("test setup: combined raw lines (%d bytes) exceed EffectiveMaxBatchBytes (%d); want them packed into one batch", len(line1)+1+len(line2), eff)
		}

		outcome, err := sink.SendBatch(context.Background(), [][]byte{line1, line2})
		if err != nil {
			t.Fatalf("SendBatch: %v", err)
		}
		if len(outcome.RejectedIndices) != 0 {
			t.Errorf("RejectedIndices = %v, want empty", outcome.RejectedIndices)
		}
		mu.Lock()
		defer mu.Unlock()
		if delivered != 2 {
			t.Fatalf("delivered = %d, want 2", delivered)
		}
		if status413Count != 0 {
			t.Errorf("server received %d HTTP 413 oversize POST(s), want 0 (proactive re-encode split must prevent oversize POST)", status413Count)
		}
		if maxSeenBody > maxBytes {
			t.Errorf("max HTTP body size = %d > maxBytes %d (proactive re-encoded bisect failed)", maxSeenBody, maxBytes)
		}
	})
}

// Scenario 1 (Option B): in dual-run mode at a small cap (16 KiB), a Hub that
// permanently rejects every event (either via per-event rejections in 200 or via
// whole-batch HTTP 400/422 PermanentSendError) must dead-letter ONLY in the Hub
// outbox. The GCP outbox must deliver 200/200 with zero overflow, zero dead
// letters, and accurate per-sink health counters (no double counting).
func TestDualAnalyticsDrainer_HubRejectionsNeverStarveGCPOrPolluteCounters(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_LAUNCH_ID", "run-reject")

	makeObs := func(i int) UsageObservation {
		return UsageObservation{
			SourceKind:      "claude.api_request",
			Identity:        telemetrycontract.SourceIdentityComposite,
			SourceEventID:   fmt.Sprintf("cmp1:rej-%d", i),
			Status:          telemetrycontract.StatusSuccess,
			Calls:           1,
			Input:           int64Ptr(int64(i)),
			Output:          int64Ptr(5),
			InputDerivation: telemetrycontract.InputDerivationReported,
			TokenStatus:     telemetrycontract.TokenStatusPartial,
		}
	}

	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "per-event rejections (HTTP 200 with all indices rejected)",
			handler: func(w http.ResponseWriter, r *http.Request) {
				var req telemetrycontract.UsageIngestRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				rej := make([]telemetrycontract.UsageIngestRejection, len(req.Events))
				for i, ev := range req.Events {
					rej[i] = telemetrycontract.UsageIngestRejection{
						Index:   i,
						EventID: ev.EventID,
						Code:    telemetrycontract.UsageRejectUnknownRun,
						Reason:  "unknown run",
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{
					Accepted: 0,
					Rejected: rej,
				})
			},
		},
		{
			name: "whole-batch HTTP 422 permanent error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte("unprocessable entity"))
			},
		},
		{
			name: "whole-batch HTTP 400 permanent error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("bad request"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			const capBytes = 16 << 10
			gcpOutbox := testOutbox(t)
			gcpOutbox.maxPending = capBytes
			hubOutbox := testOutbox(t)
			hubOutbox.maxPending = capBytes

			dual := NewDualUsageAnalytics(gcpOutbox, hubOutbox, telemetrycontract.UsageSourceNative)
			gcp := &capture{}
			sink := NewHubUsageSink(hubOutbox, HubUsageSinkConfig{
				Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
				RunID: "run-reject",
			})
			d := NewDualAnalyticsDrainer(gcpOutbox, gcp.send, sink, trustedHub("hub-1"))

			for i := 1; i <= 200; i++ {
				if out, err := dual.Record(makeObs(i)); err != nil || out != AppendWritten {
					t.Fatalf("Record(%d) = (%v, %v), want (AppendWritten, nil)", i, out, err)
				}
				d.RunOnce(context.Background())
			}

			if len(gcp.lines) != 200 {
				t.Fatalf("gcp received %d/200 events, want 200/200", len(gcp.lines))
			}
			gcpStats := gcpOutbox.Stats()
			if gcpStats.Overflow != 0 || gcpStats.PersistedOverflow != 0 {
				t.Errorf("gcp overflow = %d (persisted %d), want 0", gcpStats.Overflow, gcpStats.PersistedOverflow)
			}
			if gcpStats.DeadLetters != 0 || gcpStats.PersistedDeadLetters != 0 {
				t.Errorf("gcp deadLetters = %d (persisted %d), want 0", gcpStats.DeadLetters, gcpStats.PersistedDeadLetters)
			}
			if gcpStats.Delivered != 200 {
				t.Errorf("gcp Delivered = %d, want 200 (no double counting)", gcpStats.Delivered)
			}
			if gcpStats.PendingBytes != 0 || gcpStats.RetainedBytes != 0 {
				t.Errorf("gcp PendingBytes=%d RetainedBytes=%d, want 0", gcpStats.PendingBytes, gcpStats.RetainedBytes)
			}

			// Verify health counters reported for the GCP pipeline are completely unpolluted.
			hc := analyticsHandles{native: dual, drainer: d, send: newCountingSend(gcp.send)}.counters(time.Now())
			if hc.Delivered != 200 {
				t.Errorf("health Delivered = %d, want 200", hc.Delivered)
			}
			if hc.DeadLetters != 0 || hc.DeadLettersTotal != 0 {
				t.Errorf("health DeadLetters = %d (total %d), want 0", hc.DeadLetters, hc.DeadLettersTotal)
			}
			if hc.Overflow != 0 || hc.OverflowTotal != 0 {
				t.Errorf("health Overflow = %d (total %d), want 0", hc.Overflow, hc.OverflowTotal)
			}

			// Hub outbox absorbed all rejections and bounded itself at its own cap.
			hubStats := hubOutbox.Stats()
			if hubStats.Delivered != 0 {
				t.Errorf("hub Delivered = %d, want 0", hubStats.Delivered)
			}
			if hubStats.DeadLetters == 0 || hubStats.PersistedDeadLetters == 0 {
				t.Errorf("hub DeadLetters = %d (persisted %d), want > 0", hubStats.DeadLetters, hubStats.PersistedDeadLetters)
			}
			if hubStats.Overflow == 0 || hubStats.PersistedOverflow == 0 {
				t.Errorf("hub Overflow = %d (persisted %d), want > 0 once Hub dead_letter.jsonl hits 16 KiB cap", hubStats.Overflow, hubStats.PersistedOverflow)
			}
			if hubStats.DeadLetters+hubStats.Overflow != 200 {
				t.Errorf("hub DeadLetters(%d) + Overflow(%d) = %d, want 200", hubStats.DeadLetters, hubStats.Overflow, hubStats.DeadLetters+hubStats.Overflow)
			}
			if hubStats.RetainedBytes > capBytes+1024 {
				t.Errorf("hub RetainedBytes = %d exceeds cap %d (+finalize slack)", hubStats.RetainedBytes, capBytes)
			}
			if _, err := os.Stat(filepath.Join(gcpOutbox.Dir(), "dead_letter.jsonl")); !os.IsNotExist(err) {
				t.Errorf("GCP outbox must not have dead_letter.jsonl")
			}
			if _, err := os.Stat(filepath.Join(hubOutbox.Dir(), "dead_letter.jsonl")); err != nil {
				t.Errorf("Hub outbox must have dead_letter.jsonl: %v", err)
			}
		})
	}
}

// Scenario 2 (Option B): in dual-run mode at a small outbox cap, a persistently
// failing Hub sink (404 revoked, 500 outage, or unreachable) must never cause
// the healthy GCP Cloud Logging export to overflow or drop events, and the Hub
// outbox stays bounded by its own cap.
func TestDualAnalyticsDrainer_FailingHubNeverOverflowsGCP(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_LAUNCH_ID", "run-fail")

	makeObs := func(prefix string, i int) UsageObservation {
		return UsageObservation{
			SourceKind:      "claude.api_request",
			Identity:        telemetrycontract.SourceIdentityComposite,
			SourceEventID:   fmt.Sprintf("cmp1:%s-%d", prefix, i),
			Status:          telemetrycontract.StatusSuccess,
			Calls:           1,
			Input:           int64Ptr(int64(i)),
			Output:          int64Ptr(5),
			InputDerivation: telemetrycontract.InputDerivationReported,
			TokenStatus:     telemetrycontract.TokenStatusPartial,
		}
	}

	t.Run("404 capability revoked stops Hub sink and GCP receives 200/200", func(t *testing.T) {
		var hubCalls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hubCalls.Add(1)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("not found"))
		}))
		defer srv.Close()

		gcpOutbox := testOutbox(t)
		gcpOutbox.maxPending = 16 << 10
		hubOutbox := testOutbox(t)
		hubOutbox.maxPending = 16 << 10

		dual := NewDualUsageAnalytics(gcpOutbox, hubOutbox, telemetrycontract.UsageSourceNative)
		gcp := &capture{}
		sink := NewHubUsageSink(hubOutbox, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-404",
		})
		d := NewDualAnalyticsDrainer(gcpOutbox, gcp.send, sink, trustedHub("hub-1"))
		d.onHubRevoked = func() { dual.clearHubOutbox() }

		for i := 1; i <= 200; i++ {
			if out, err := dual.Record(makeObs("rev", i)); err != nil || out != AppendWritten {
				t.Fatalf("Record(%d) = (%v, %v), want (AppendWritten, nil)", i, out, err)
			}
			d.RunOnce(context.Background())
		}

		if !d.HubRevoked() {
			t.Fatal("expected HubRevoked() = true after HTTP 404")
		}
		if got := hubCalls.Load(); got != 1 {
			t.Errorf("hubCalls = %d, want 1 (must stop after first 404, not retry forever)", got)
		}
		if isHubOutboxActiveDir(hubOutbox.Dir()) {
			t.Error("expected Hub outbox active marker to be removed on 404 revocation")
		}
		if dual.HubOutbox() != nil {
			t.Error("expected native producer to detach Hub outbox after 404 revocation")
		}
		if len(gcp.lines) != 200 {
			t.Errorf("gcp received %d/200 events, want 200/200", len(gcp.lines))
		}
		stats := gcpOutbox.Stats()
		if stats.Overflow != 0 || stats.PersistedOverflow != 0 {
			t.Errorf("gcp overflow = %d (persisted %d), want 0", stats.Overflow, stats.PersistedOverflow)
		}
		if stats.PendingBytes != 0 {
			t.Errorf("gcp PendingBytes = %d, want 0", stats.PendingBytes)
		}
	})

	t.Run("Hub-only 404 stops draining rather than retrying forever", func(t *testing.T) {
		var hubCalls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hubCalls.Add(1)
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		hubOutbox := testOutbox(t)
		mustAppend(t, hubOutbox, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:hubonly-404", 1), AppendWritten)
		sink := NewHubUsageSink(hubOutbox, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-hubonly-404",
		})
		d := NewHubAnalyticsDrainer(hubOutbox, sink, trustedHub("hub-1"))
		clock := time.Date(2026, 11, 25, 20, 0, 0, 0, time.UTC)
		d.now = func() time.Time { return clock }

		d.RunOnce(context.Background())
		clock = clock.Add(10 * time.Minute)
		d.RunOnce(context.Background())
		clock = clock.Add(10 * time.Minute)
		d.RunOnce(context.Background())

		if !d.HubRevoked() {
			t.Fatal("expected HubRevoked() = true after 404 in Hub-only mode")
		}
		if isHubOutboxActiveDir(hubOutbox.Dir()) {
			t.Error("expected Hub outbox active marker to be removed on 404 in Hub-only mode")
		}
		if got := hubCalls.Load(); got != 1 {
			t.Errorf("hubCalls = %d, want 1 (must not retry forever)", got)
		}
	})

	t.Run("persistent 500 Hub overflows only Hub outbox and GCP receives 200/200", func(t *testing.T) {
		var hubDown atomic.Bool
		hubDown.Store(true)
		var mu sync.Mutex
		var recoveredReqs []telemetrycontract.UsageIngestRequest

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if hubDown.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("down"))
				return
			}
			var req telemetrycontract.UsageIngestRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			recoveredReqs = append(recoveredReqs, req)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
		}))
		defer srv.Close()

		const capBytes = 16 << 10
		gcpOutbox := testOutbox(t)
		gcpOutbox.maxPending = capBytes
		hubOutbox := testOutbox(t)
		hubOutbox.maxPending = capBytes

		dual := NewDualUsageAnalytics(gcpOutbox, hubOutbox, telemetrycontract.UsageSourceNative)
		gcp := &capture{}
		sink := NewHubUsageSink(hubOutbox, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-500",
		})
		d := NewDualAnalyticsDrainer(gcpOutbox, gcp.send, sink, trustedHub("hub-1"))
		clock := time.Date(2026, 11, 25, 20, 0, 0, 0, time.UTC)
		d.now = func() time.Time { return clock }

		for i := 1; i <= 200; i++ {
			if out, err := dual.Record(makeObs("500", i)); err != nil || out != AppendWritten {
				t.Fatalf("Record(%d) = (%v, %v), want (AppendWritten, nil)", i, out, err)
			}
			d.RunOnce(context.Background())
		}

		if len(gcp.lines) != 200 {
			t.Fatalf("gcp received %d/200 events while Hub returned 500, want 200/200", len(gcp.lines))
		}
		gcpStats := gcpOutbox.Stats()
		if gcpStats.Overflow != 0 || gcpStats.PersistedOverflow != 0 {
			t.Errorf("gcp overflow = %d (persisted %d), want 0", gcpStats.Overflow, gcpStats.PersistedOverflow)
		}
		if gcpStats.PendingBytes != 0 {
			t.Errorf("gcp PendingBytes = %d, want 0", gcpStats.PendingBytes)
		}
		if d.Backoff() != 0 {
			t.Errorf("GCP Backoff() = %v, want 0 while GCP is healthy", d.Backoff())
		}
		if d.HubBackoff() == 0 {
			t.Error("HubBackoff() = 0, want > 0 while Hub is failing")
		}

		hubStats := hubOutbox.Stats()
		if hubStats.Overflow == 0 || hubStats.PersistedOverflow == 0 {
			t.Errorf("hub overflow = %d (persisted %d), want > 0 at 16 KiB cap", hubStats.Overflow, hubStats.PersistedOverflow)
		}
		if hubStats.RetainedBytes > capBytes+1024 {
			t.Errorf("hub RetainedBytes = %d exceeds cap %d (+finalize slack)", hubStats.RetainedBytes, capBytes)
		}

		// When Hub recovers, it drains its buffered events with no MissingRanges
		// (Hub is sole authority on meter_health gaps via seqs).
		hubDown.Store(false)
		clock = clock.Add(10 * time.Minute)
		d.RunOnce(context.Background())

		mu.Lock()
		defer mu.Unlock()
		if len(recoveredReqs) == 0 {
			t.Fatal("expected Hub request after recovery")
		}
		for _, req := range recoveredReqs {
			if len(req.MissingRanges) != 0 {
				t.Errorf("client must not send MissingRanges (Hub is sole gap authority), got %+v", req.MissingRanges)
			}
		}
		if hubOutbox.Stats().PendingBytes != 0 {
			t.Errorf("hub PendingBytes = %d after recovery, want 0", hubOutbox.Stats().PendingBytes)
		}
	})

	t.Run("unreachable Hub overflows only Hub outbox and GCP receives 200/200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		closedURL := srv.URL
		srv.Close() // immediately unreachable (connection refused)

		const capBytes = 16 << 10
		gcpOutbox := testOutbox(t)
		gcpOutbox.maxPending = capBytes
		hubOutbox := testOutbox(t)
		hubOutbox.maxPending = capBytes

		dual := NewDualUsageAnalytics(gcpOutbox, hubOutbox, telemetrycontract.UsageSourceNative)
		gcp := &capture{}
		sink := NewHubUsageSink(hubOutbox, HubUsageSinkConfig{
			Post:  httptestPostFunc(closedURL, "agent-1", "tok-1"),
			RunID: "run-unreach",
		})
		d := NewDualAnalyticsDrainer(gcpOutbox, gcp.send, sink, trustedHub("hub-1"))

		for i := 1; i <= 200; i++ {
			if out, err := dual.Record(makeObs("unreach", i)); err != nil || out != AppendWritten {
				t.Fatalf("Record(%d) = (%v, %v), want (AppendWritten, nil)", i, out, err)
			}
			d.RunOnce(context.Background())
		}

		if len(gcp.lines) != 200 {
			t.Fatalf("gcp received %d/200 events while Hub was unreachable, want 200/200", len(gcp.lines))
		}
		if gcpOutbox.Stats().Overflow != 0 {
			t.Errorf("gcp overflow = %d, want 0", gcpOutbox.Stats().Overflow)
		}
		hubStats := hubOutbox.Stats()
		if hubStats.Overflow == 0 {
			t.Errorf("hub overflow = %d, want > 0", hubStats.Overflow)
		}
		if hubStats.RetainedBytes > capBytes+1024 {
			t.Errorf("hub RetainedBytes = %d exceeds cap %d (+finalize slack)", hubStats.RetainedBytes, capBytes)
		}
	})
}

// Scenario 3 (Option B): appends from a second process (simulating short-lived
// `sciontool hook` subprocesses via separate NewLazyHookUsageAnalytics /
// OpenAnalyticsOutbox handles) tee into both active outboxes, respect each cap
// independently, and react to Hub active marker creation/removal on disk.
func TestDualAnalyticsDrainer_SecondProcessHookAppendsTeeAndRespectCaps(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_LAUNCH_ID", "run-hooks")

	makeObs := func(prefix string, i int) UsageObservation {
		return UsageObservation{
			SourceKind:      "claude.api_request",
			Identity:        telemetrycontract.SourceIdentityComposite,
			SourceEventID:   fmt.Sprintf("cmp1:%s-%d", prefix, i),
			Status:          telemetrycontract.StatusSuccess,
			Calls:           1,
			Input:           int64Ptr(int64(i)),
			Output:          int64Ptr(5),
			InputDerivation: telemetrycontract.InputDerivationReported,
			TokenStatus:     telemetrycontract.TokenStatusPartial,
		}
	}

	t.Run("hook subprocesses tee to both outboxes when both healthy", func(t *testing.T) {
		var mu sync.Mutex
		var hubIDs []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req telemetrycontract.UsageIngestRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			for _, ev := range req.Events {
				hubIDs = append(hubIDs, ev.EventID)
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
		}))
		defer srv.Close()

		home := t.TempDir()
		gcpOutbox, err := OpenAnalyticsOutbox(AnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox(gcp): %v", err)
		}
		hubOutbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox(hub): %v", err)
		}
		gcp := &capture{}
		sink := NewHubUsageSink(hubOutbox, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-hooks",
		})
		d := NewDualAnalyticsDrainer(gcpOutbox, gcp.send, sink, trustedHub("hub-1"))

		// Each iteration creates a fresh lazy hook handle (simulating separate processes).
		for i := 1; i <= 5; i++ {
			hook := NewLazyHookUsageAnalytics(home, true, nil)
			if out, err := hook.Record(makeObs("hk-ok", i)); err != nil || out != AppendWritten {
				t.Fatalf("hook.Record(%d) = (%v, %v), want (AppendWritten, nil)", i, out, err)
			}
		}

		d.RunOnce(context.Background())

		if len(gcp.lines) != 5 {
			t.Fatalf("gcp received %d/5 events, want 5", len(gcp.lines))
		}
		mu.Lock()
		gotHub := append([]string(nil), hubIDs...)
		mu.Unlock()
		if len(gotHub) != 5 {
			t.Fatalf("hub received %d/5 events, want 5", len(gotHub))
		}
		for i := range gcp.lines {
			if gotHub[i] != decodeUsage(t, gcp.lines[i]).EventID {
				t.Errorf("event[%d] mismatch: gcp=%q hub=%q", i, decodeUsage(t, gcp.lines[i]).EventID, gotHub[i])
			}
		}
	})

	t.Run("hook subprocesses under failing Hub respect each cap independently", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		const capBytes = 16 << 10
		home := t.TempDir()
		gcpOutbox, err := OpenAnalyticsOutbox(AnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox(gcp): %v", err)
		}
		gcpOutbox.maxPending = capBytes
		hubOutbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox(hub): %v", err)
		}
		hubOutbox.maxPending = capBytes

		gcp := &capture{}
		sink := NewHubUsageSink(hubOutbox, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-hooks-500",
		})
		d := NewDualAnalyticsDrainer(gcpOutbox, gcp.send, sink, trustedHub("hub-1"))

		// Separate outbox handles (simulating a second process) with the same 16 KiB cap.
		gcpHookOutbox, err := OpenAnalyticsOutbox(AnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox(gcpHook): %v", err)
		}
		gcpHookOutbox.maxPending = capBytes
		hubHookOutbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox(hubHook): %v", err)
		}
		hubHookOutbox.maxPending = capBytes
		hookDual := NewDualUsageAnalytics(gcpHookOutbox, hubHookOutbox, telemetrycontract.UsageSourceHooks)

		for i := 1; i <= 200; i++ {
			if out, err := hookDual.Record(makeObs("hk-500", i)); err != nil || out != AppendWritten {
				t.Fatalf("hookDual.Record(%d) = (%v, %v), want (AppendWritten, nil)", i, out, err)
			}
			d.RunOnce(context.Background())
		}

		if len(gcp.lines) != 200 {
			t.Fatalf("gcp received %d/200 events, want 200/200", len(gcp.lines))
		}
		if gcpOutbox.Stats().PersistedOverflow != 0 {
			t.Errorf("gcp PersistedOverflow = %d, want 0", gcpOutbox.Stats().PersistedOverflow)
		}
		hubStats := hubOutbox.Stats()
		if hubStats.PersistedOverflow == 0 {
			t.Errorf("hub PersistedOverflow = %d, want > 0 at 16 KiB cap", hubStats.PersistedOverflow)
		}
		if hubStats.RetainedBytes > capBytes+1024 {
			t.Errorf("hub RetainedBytes = %d exceeds cap %d (+finalize slack)", hubStats.RetainedBytes, capBytes)
		}
	})

	t.Run("hook subprocesses stop teeing to Hub after 404 revocation and respect Hub-only mode", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		home := t.TempDir()
		gcpOutbox, err := OpenAnalyticsOutbox(AnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox(gcp): %v", err)
		}
		hubOutbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox(hub): %v", err)
		}
		gcp := &capture{}
		sink := NewHubUsageSink(hubOutbox, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-hooks-404",
		})
		d := NewDualAnalyticsDrainer(gcpOutbox, gcp.send, sink, trustedHub("hub-1"))

		// Before 404: hook writes to both.
		hook1 := NewLazyHookUsageAnalytics(home, true, nil)
		if _, err := hook1.Record(makeObs("hk-rev", 1)); err != nil {
			t.Fatalf("hook1.Record: %v", err)
		}
		d.RunOnce(context.Background())
		if !d.HubRevoked() {
			t.Fatal("expected HubRevoked() = true")
		}
		seqBefore := hubOutbox.Stats().Appended
		ledgerBefore, err := hubOutbox.LedgerSnapshot()
		if err != nil {
			t.Fatalf("LedgerSnapshot before: %v", err)
		}

		// After 404: active marker is gone, so a new hook subprocess writes ONLY to GCP.
		hook2 := NewLazyHookUsageAnalytics(home, true, nil)
		if _, err := hook2.Record(makeObs("hk-rev", 2)); err != nil {
			t.Fatalf("hook2.Record: %v", err)
		}
		d.RunOnce(context.Background())
		if len(gcp.lines) != 2 {
			t.Fatalf("gcp received %d/2 events, want 2", len(gcp.lines))
		}
		ledgerAfter, err := hubOutbox.LedgerSnapshot()
		if err != nil {
			t.Fatalf("LedgerSnapshot after: %v", err)
		}
		if ledgerAfter.LastSeq != ledgerBefore.LastSeq || hubOutbox.Stats().Appended != seqBefore {
			t.Errorf("Hub outbox received append after 404 revocation: seq %d -> %d", ledgerBefore.LastSeq, ledgerAfter.LastSeq)
		}

		// Hub-only hook after revocation is a no-op and does not error or append.
		hubOnlyHook := NewLazyHookUsageAnalytics(home, false, nil)
		if _, err := hubOnlyHook.Record(makeObs("hk-rev", 3)); err != nil {
			t.Errorf("hubOnlyHook.Record after revocation returned err: %v", err)
		}
		ledgerFinal, err := hubOutbox.LedgerSnapshot()
		if err != nil {
			t.Fatalf("LedgerSnapshot final: %v", err)
		}
		if ledgerFinal.LastSeq != ledgerBefore.LastSeq {
			t.Errorf("Hub-only hook appended after 404 revocation: seq %d -> %d", ledgerBefore.LastSeq, ledgerFinal.LastSeq)
		}
	})

	t.Run("Hub-only hook subprocess writes only to Hub outbox and never creates GCP outbox", func(t *testing.T) {
		var mu sync.Mutex
		var delivered int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req telemetrycontract.UsageIngestRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			delivered += len(req.Events)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
		}))
		defer srv.Close()

		home := t.TempDir()
		hubOutbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox(hub): %v", err)
		}
		sink := NewHubUsageSink(hubOutbox, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-hubonly-hook",
		})
		d := NewHubAnalyticsDrainer(hubOutbox, sink, trustedHub("hub-1"))

		hook := NewLazyHookUsageAnalytics(home, false, nil)
		if out, err := hook.Record(makeObs("hk-hubonly", 1)); err != nil || out != AppendWritten {
			t.Fatalf("hook.Record = (%v, %v), want (AppendWritten, nil)", out, err)
		}
		d.RunOnce(context.Background())

		if _, err := os.Stat(AnalyticsOutboxDirFor(home)); !os.IsNotExist(err) {
			t.Errorf("Hub-only hook must not create GCP outbox dir %q", AnalyticsOutboxDirFor(home))
		}
		mu.Lock()
		defer mu.Unlock()
		if delivered != 1 {
			t.Errorf("delivered = %d, want 1", delivered)
		}
	})
}

// Scenario 4 (Option B): periodic re-probe from a GCP-only drainer creates no
// Hub outbox while probe is negative; when capabilities turn positive, it opens
// the sibling Hub outbox, marks it active on disk, and wires both native and
// hook producers to tee into both outboxes without disrupting GCP delivery.
func TestDualAnalyticsDrainer_PeriodicReprobeActivatesHubSink(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_LAUNCH_ID", "run-reprobe")

	var probeCalls atomic.Int32
	var mu sync.Mutex
	var hubDelivered []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		for _, ev := range req.Events {
			hubDelivered = append(hubDelivered, ev.EventID)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
	}))
	defer srv.Close()

	home := t.TempDir()
	gcpOutbox, err := OpenAnalyticsOutbox(AnalyticsOutboxDirFor(home))
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(gcp): %v", err)
	}
	native := NewUsageAnalytics(gcpOutbox, telemetrycontract.UsageSourceNative)
	gcp := &capture{}
	d := NewAnalyticsDrainer(gcpOutbox, gcp.send, trustedHub("hub-1"))
	d.hubOutboxDir = HubAnalyticsOutboxDirFor(home)
	clock := time.Date(2026, 11, 25, 21, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return clock }

	d.SetHubProbe(func(ctx context.Context) (*HubUsageSinkConfig, error) {
		if probeCalls.Add(1) == 1 {
			return nil, errors.New("hub 503")
		}
		return &HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-reprobe",
		}, nil
	}, 60*time.Second, nil, nil, func(cfg HubUsageSinkConfig, hubOutbox *AnalyticsOutbox, sink *HubUsageSink) {
		native.setHubOutbox(hubOutbox, cfg.RunID, sink.NoteActivity)
	})

	makeObs := func(id string) UsageObservation {
		return UsageObservation{
			SourceKind:      "claude.api_request",
			Identity:        telemetrycontract.SourceIdentityComposite,
			SourceEventID:   id,
			Status:          telemetrycontract.StatusSuccess,
			Calls:           1,
			Input:           int64Ptr(10),
			InputDerivation: telemetrycontract.InputDerivationReported,
			TokenStatus:     telemetrycontract.TokenStatusPartial,
		}
	}

	// Tick 1 (t=0): probe #1 returns 503; no Hub outbox created; GCP delivers event 1.
	if _, err := native.Record(makeObs("cmp1:rp-1")); err != nil {
		t.Fatalf("Record 1: %v", err)
	}
	d.RunOnce(context.Background())
	if len(gcp.lines) != 1 {
		t.Fatalf("gcp lines = %d, want 1", len(gcp.lines))
	}
	if _, err := os.Stat(HubAnalyticsOutboxDirFor(home)); !os.IsNotExist(err) {
		t.Fatal("Hub outbox dir must not be created while probe is negative")
	}

	// Tick 2 (t+60s): probe #2 returns 200; Hub outbox is created & marked active.
	clock = clock.Add(60 * time.Second)
	d.RunOnce(context.Background())
	if !IsHubOutboxActive(home) {
		t.Fatal("expected Hub outbox active marker after positive re-probe")
	}

	// Record event 2 via native producer and event 3 via a fresh hook subprocess handle.
	if _, err := native.Record(makeObs("cmp1:rp-2")); err != nil {
		t.Fatalf("Record 2: %v", err)
	}
	hook := NewLazyHookUsageAnalytics(home, true, nil)
	if _, err := hook.Record(makeObs("cmp1:rp-3")); err != nil {
		t.Fatalf("hook.Record 3: %v", err)
	}
	d.RunOnce(context.Background())

	if len(gcp.lines) != 3 {
		t.Fatalf("gcp lines = %d, want 3", len(gcp.lines))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hubDelivered) != 2 {
		t.Fatalf("hubDelivered = %v, want 2 events (rp-2 and rp-3)", hubDelivered)
	}
	if hubDelivered[0] != decodeUsage(t, gcp.lines[1]).EventID || hubDelivered[1] != decodeUsage(t, gcp.lines[2]).EventID {
		t.Errorf("hubDelivered = %v, want [%q, %q]", hubDelivered, decodeUsage(t, gcp.lines[1]).EventID, decodeUsage(t, gcp.lines[2]).EventID)
	}
}

// O1: native and hook appenders must share one run identity source
// (SCION_LAUNCH_ID preferred over a disagreeing probe RunID) so alternating
// native and hook appends never oscillate producer_epoch and every event
// carries the container's own run_id.
func TestHubUsageSink_SingleRunIDSourceNoEpochOscillation(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_LAUNCH_ID", "run-container")

	var mu sync.Mutex
	var delivered []telemetrycontract.UsageEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		delivered = append(delivered, req.Events...)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
	}))
	defer srv.Close()

	dir := filepath.Join(t.TempDir(), "outbox")
	oNative, err := OpenAnalyticsOutbox(dir)
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(native): %v", err)
	}
	oHook, err := OpenAnalyticsOutbox(dir)
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(hook): %v", err)
	}

	native := NewUsageAnalytics(oNative, telemetrycontract.UsageSourceNative)
	// Simulate a probe returning a different fallback RunID; SCION_LAUNCH_ID must win.
	native.setRunAndActivity("run-probe-different", nil)
	hook := NewUsageAnalytics(oHook, telemetrycontract.UsageSourceHooks)

	obs := func(id string) UsageObservation {
		return UsageObservation{
			SourceKind:      "claude.api_request",
			Identity:        telemetrycontract.SourceIdentityComposite,
			SourceEventID:   id,
			Status:          telemetrycontract.StatusSuccess,
			Calls:           1,
			Input:           int64Ptr(10),
			InputDerivation: telemetrycontract.InputDerivationReported,
			TokenStatus:     telemetrycontract.TokenStatusPartial,
		}
	}

	// Alternate native and hook appends.
	for i, pair := range []struct {
		p  *UsageAnalytics
		id string
	}{
		{native, "cmp1:alt-1"},
		{hook, "cmp1:alt-2"},
		{native, "cmp1:alt-3"},
		{hook, "cmp1:alt-4"},
	} {
		if _, err := pair.p.Record(obs(pair.id)); err != nil {
			t.Fatalf("Record[%d]: %v", i, err)
		}
	}

	sink := NewHubUsageSink(oNative, HubUsageSinkConfig{
		Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
		RunID: "run-container",
	})
	d := NewHubAnalyticsDrainer(oNative, sink, trustedHub("hub-1"))
	if sent := d.RunOnce(context.Background()); sent != 4 {
		t.Fatalf("sent = %d, want 4", sent)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 4 {
		t.Fatalf("delivered = %d, want 4", len(delivered))
	}
	epoch0 := delivered[0].ProducerEpoch
	for i, ev := range delivered {
		if ev.RunID != "run-container" {
			t.Errorf("event[%d] RunID = %q, want %q", i, ev.RunID, "run-container")
		}
		if ev.ProducerEpoch != epoch0 {
			t.Errorf("event[%d] ProducerEpoch = %q, want constant %q (epoch oscillated!)", i, ev.ProducerEpoch, epoch0)
		}
		if ev.ProducerSeq != int64(i+1) {
			t.Errorf("event[%d] ProducerSeq = %d, want %d", i, ev.ProducerSeq, i+1)
		}
	}
}

// Scenario 5 (Option B): kill/restart replay from BOTH outboxes across a run
// change keeps stable event IDs and preserves the original run_id for replayed
// events while rotating the producer epoch for the new run.
func TestDualOutbox_KillRestartReplayPreservesEventIDAndRunID(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_HARNESS", "claude")

	makeObs := func(id string) UsageObservation {
		return UsageObservation{
			SourceKind:      "claude.api_request",
			Identity:        telemetrycontract.SourceIdentityComposite,
			SourceEventID:   id,
			Status:          telemetrycontract.StatusSuccess,
			Calls:           1,
			Input:           int64Ptr(42),
			Output:          int64Ptr(7),
			InputDerivation: telemetrycontract.InputDerivationReported,
			TokenStatus:     telemetrycontract.TokenStatusPartial,
		}
	}

	home := t.TempDir()
	gcpDir := AnalyticsOutboxDirFor(home)
	hubDir := HubAnalyticsOutboxDirFor(home)

	// Process 1 (run-1): append 2 events to both outboxes, then crash before delivery.
	t.Setenv("SCION_LAUNCH_ID", "run-1")
	gcp1, err := OpenAnalyticsOutbox(gcpDir)
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(gcp1): %v", err)
	}
	hub1, err := OpenAnalyticsOutbox(hubDir)
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(hub1): %v", err)
	}
	dual1 := NewDualUsageAnalytics(gcp1, hub1, telemetrycontract.UsageSourceNative)
	if _, err := dual1.Record(makeObs("cmp1:kr-1")); err != nil {
		t.Fatalf("Record 1: %v", err)
	}
	if _, err := dual1.Record(makeObs("cmp1:kr-2")); err != nil {
		t.Fatalf("Record 2: %v", err)
	}
	// Fail both sinks on tick 1 so ready files stay undelivered on disk.
	gcpFail := &capture{fail: errors.New("gcp down")}
	downSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	sink1 := NewHubUsageSink(hub1, HubUsageSinkConfig{
		Post:  httptestPostFunc(downSrv.URL, "agent-1", "tok-1"),
		RunID: "run-1",
	})
	d1 := NewDualAnalyticsDrainer(gcp1, gcpFail.send, sink1, trustedHub("hub-1"))
	d1.RunOnce(context.Background())
	downSrv.Close()

	// Process 2 (run-2): reopen both outboxes from disk, append 1 new event in run-2, drain both.
	t.Setenv("SCION_LAUNCH_ID", "run-2")
	var mu sync.Mutex
	var hubReqs []telemetrycontract.UsageIngestRequest
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		hubReqs = append(hubReqs, req)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
	}))
	defer srv2.Close()

	gcp2, err := OpenAnalyticsOutbox(gcpDir)
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(gcp2): %v", err)
	}
	hub2, err := OpenAnalyticsOutbox(hubDir)
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(hub2): %v", err)
	}
	dual2 := NewDualUsageAnalytics(gcp2, hub2, telemetrycontract.UsageSourceNative)
	if _, err := dual2.Record(makeObs("cmp1:kr-3")); err != nil {
		t.Fatalf("Record 3: %v", err)
	}

	gcpOK := &capture{}
	sink2 := NewHubUsageSink(hub2, HubUsageSinkConfig{
		Post:  httptestPostFunc(srv2.URL, "agent-1", "tok-1"),
		RunID: "run-2",
	})
	d2 := NewDualAnalyticsDrainer(gcp2, gcpOK.send, sink2, trustedHub("hub-1"))
	d2.RunOnce(context.Background())

	if len(gcpOK.lines) != 3 {
		t.Fatalf("gcp replayed %d events, want 3", len(gcpOK.lines))
	}
	gcpEvs := make([]telemetrycontract.UsageEvent, 3)
	for i, line := range gcpOK.lines {
		gcpEvs[i] = decodeUsage(t, line)
	}

	mu.Lock()
	var hubEvs []telemetrycontract.UsageEvent
	for _, req := range hubReqs {
		for _, ev := range req.Events {
			// Request-level RunID must match every event's RunID (grouped by run_id, epoch).
			if req.RunID != ev.RunID {
				t.Errorf("hub req.RunID = %q != ev.RunID = %q", req.RunID, ev.RunID)
			}
			hubEvs = append(hubEvs, ev)
		}
	}
	mu.Unlock()
	if len(hubEvs) != 3 {
		t.Fatalf("hub replayed %d events, want 3", len(hubEvs))
	}

	// Verify stable EventIDs across GCP and Hub, preserved run-1 on replayed events,
	// and rotated epoch on run-2 in both outboxes.
	for i := 0; i < 3; i++ {
		if gcpEvs[i].EventID != hubEvs[i].EventID {
			t.Errorf("event[%d] EventID mismatch: gcp=%q hub=%q", i, gcpEvs[i].EventID, hubEvs[i].EventID)
		}
	}
	for _, label := range []struct {
		name string
		evs  []telemetrycontract.UsageEvent
	}{
		{"gcp", gcpEvs},
		{"hub", hubEvs},
	} {
		if label.evs[0].RunID != "run-1" || label.evs[1].RunID != "run-1" || label.evs[2].RunID != "run-2" {
			t.Errorf("%s run_ids = [%q, %q, %q], want [run-1, run-1, run-2]", label.name, label.evs[0].RunID, label.evs[1].RunID, label.evs[2].RunID)
		}
		if label.evs[0].ProducerEpoch != label.evs[1].ProducerEpoch {
			t.Errorf("%s run-1 epochs differ: %q vs %q", label.name, label.evs[0].ProducerEpoch, label.evs[1].ProducerEpoch)
		}
		if label.evs[2].ProducerEpoch == label.evs[0].ProducerEpoch {
			t.Errorf("%s run-2 did not rotate ProducerEpoch (%q)", label.name, label.evs[2].ProducerEpoch)
		}
		if label.evs[2].ProducerSeq != 1 {
			t.Errorf("%s run-2 ProducerSeq = %d, want 1", label.name, label.evs[2].ProducerSeq)
		}
	}
}

// Restarting from dual-run into Hub-only mode while Hub was behind GCP resumes
// cleanly from the independent Hub outbox without skipping any events.
func TestDualToHubOnlyRestart_IndependentHubOutboxResumesCleanly(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_LAUNCH_ID", "run-o4")

	home := t.TempDir()
	gcpDir := AnalyticsOutboxDirFor(home)
	hubDir := HubAnalyticsOutboxDirFor(home)

	var hubDown atomic.Bool
	hubDown.Store(true)
	var mu sync.Mutex
	var hubIDs []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hubDown.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		for _, ev := range req.Events {
			hubIDs = append(hubIDs, ev.EventID)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
	}))
	defer srv.Close()

	// Process 1: dual-run; GCP drains all 3 events, Hub fails so Hub outbox keeps them.
	gcp1, err := OpenAnalyticsOutbox(gcpDir)
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(gcp1): %v", err)
	}
	hub1, err := OpenAnalyticsOutbox(hubDir)
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(hub1): %v", err)
	}
	dual1 := NewDualUsageAnalytics(gcp1, hub1, telemetrycontract.UsageSourceNative)
	for i := 1; i <= 3; i++ {
		if _, err := dual1.Record(UsageObservation{
			SourceKind:      "claude.api_request",
			Identity:        telemetrycontract.SourceIdentityComposite,
			SourceEventID:   fmt.Sprintf("cmp1:o4-%d", i),
			Status:          telemetrycontract.StatusSuccess,
			Calls:           1,
			Input:           int64Ptr(int64(i)),
			InputDerivation: telemetrycontract.InputDerivationReported,
			TokenStatus:     telemetrycontract.TokenStatusPartial,
		}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	gcp := &capture{}
	sink1 := NewHubUsageSink(hub1, HubUsageSinkConfig{
		Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
		RunID: "run-o4",
	})
	d1 := NewDualAnalyticsDrainer(gcp1, gcp.send, sink1, trustedHub("hub-1"))
	d1.RunOnce(context.Background())
	if len(gcp.lines) != 3 {
		t.Fatalf("gcp received %d, want 3", len(gcp.lines))
	}

	// Process 2: restarts in Hub-only mode (GCP config removed); Hub recovers and delivers all 3.
	hubDown.Store(false)
	hub2, err := OpenAnalyticsOutbox(hubDir)
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox(hub2): %v", err)
	}
	sink2 := NewHubUsageSink(hub2, HubUsageSinkConfig{
		Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
		RunID: "run-o4",
	})
	d2 := NewHubAnalyticsDrainer(hub2, sink2, trustedHub("hub-1"))
	if sent := d2.RunOnce(context.Background()); sent != 3 {
		t.Fatalf("Hub-only restart sent %d, want 3", sent)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hubIDs) != 3 {
		t.Fatalf("hubIDs = %v, want 3 events", hubIDs)
	}
	for i := range gcp.lines {
		if hubIDs[i] != decodeUsage(t, gcp.lines[i]).EventID {
			t.Errorf("hubIDs[%d] = %q, want %q", i, hubIDs[i], decodeUsage(t, gcp.lines[i]).EventID)
		}
	}
}

// O3: both Drain (legacy/GCP path) and DrainBatch must rename a truncated
// ready-N file to quarantine-N BEFORE advancing the cursor to (N+1, 0), so a
// crash between the two steps never leaves ready-N behind an advanced cursor
// where a later drain would delete it instead of keeping the quarantined bytes.
func TestDrainQuarantineOrder_RenamesBeforeCursor(t *testing.T) {
	runCase := func(t *testing.T, drainFn func(o *AnalyticsOutbox) error) {
		o := testOutbox(t)
		// Write a truncated ready-1.jsonl (no trailing newline).
		if err := os.WriteFile(filepath.Join(o.Dir(), "ready-1.jsonl"), []byte(`{"partial":true`), 0o600); err != nil {
			t.Fatalf("write truncated ready-1: %v", err)
		}
		// Seed a valid ready-2.jsonl so we can verify recovery after crash-between-steps.
		ev2 := draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:q-2", 2)
		ev2.EventID = "u1-q-2"
		ev2.HubID = "hub-1"
		ev2.ProducerEpoch = "ep-1"
		ev2.ProducerSeq = 2
		ev2.SourceOccurrence = 1
		line2, err := telemetrycontract.Marshal(ev2)
		if err != nil {
			t.Fatalf("marshal ev2: %v", err)
		}
		if err := os.WriteFile(filepath.Join(o.Dir(), "ready-2.jsonl"), append(line2, '\n'), 0o600); err != nil {
			t.Fatalf("write ready-2: %v", err)
		}

		// Force cursor write to fail by turning "cursor" into a directory
		// after readCursor (which succeeds when cursor does not exist yet —
		// wait: readCursor returns an error on a directory! So inject the
		// failure by making cursor a non-empty directory only inside openFile
		// right before EOF, so readCursor succeeds on entry and writeCursor
		// fails when Drain tries to advance the cursor).
		origOpen := o.openFile
		o.openFile = func(name string) (readyFile, error) {
			f, err := origOpen(name)
			if err == nil && filepath.Base(name) == "ready-1.jsonl" {
				// Block os.Rename(tmp, "cursor") in writeCursor by creating a directory at "cursor".
				if mkErr := os.Mkdir(filepath.Join(o.Dir(), "cursor"), 0o700); mkErr != nil {
					t.Fatalf("mkdir cursor blocker: %v", mkErr)
				}
			}
			return f, err
		}

		if err := drainFn(o); err == nil {
			t.Fatal("expected drain to fail when cursor write is blocked")
		}

		// Because quarantine rename happens BEFORE cursor write, quarantine-1.jsonl
		// must already exist on disk and ready-1.jsonl must be gone.
		if _, err := os.Stat(filepath.Join(o.Dir(), "quarantine-1.jsonl")); err != nil {
			t.Fatalf("expected quarantine-1.jsonl to exist before cursor write failed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(o.Dir(), "ready-1.jsonl")); !os.IsNotExist(err) {
			t.Fatalf("expected ready-1.jsonl to be renamed away, stat err = %v", err)
		}

		// Simulate recovery after the crash between rename and cursor write:
		// remove the cursor blocker and drain again; quarantine-1.jsonl stays
		// preserved and ready-2.jsonl delivers cleanly.
		o.openFile = origOpen
		if err := os.Remove(filepath.Join(o.Dir(), "cursor")); err != nil {
			t.Fatalf("remove cursor blocker: %v", err)
		}
		if err := drainFn(o); err != nil {
			t.Fatalf("recovery drain failed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(o.Dir(), "quarantine-1.jsonl")); err != nil {
			t.Errorf("quarantine-1.jsonl must be preserved after recovery: %v", err)
		}
		if _, err := os.Stat(filepath.Join(o.Dir(), "ready-2.jsonl")); !os.IsNotExist(err) {
			t.Errorf("ready-2.jsonl should be delivered and removed after recovery, stat err = %v", err)
		}
	}

	t.Run("legacy Drain", func(t *testing.T) {
		runCase(t, func(o *AnalyticsOutbox) error {
			_, err := o.Drain(context.Background(), func(context.Context, []byte) error { return nil })
			return err
		})
	})

	t.Run("DrainBatch", func(t *testing.T) {
		runCase(t, func(o *AnalyticsOutbox) error {
			_, err := o.DrainBatch(context.Background(), 10, 64<<10, func(context.Context, [][]byte) (BatchSendOutcome, error) {
				return BatchSendOutcome{}, nil
			})
			return err
		})
	})
}
