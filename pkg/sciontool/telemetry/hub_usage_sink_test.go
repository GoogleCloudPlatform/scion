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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	// Verify hook helper also enables when EnvUsageHubIngest=1 without GCP exporter.
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
