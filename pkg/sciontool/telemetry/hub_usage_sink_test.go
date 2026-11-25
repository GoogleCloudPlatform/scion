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

// R4: dual-run fan-out with independent cursors. When both GCP Cloud Logging
// and Hub sink are active, both receive every event, and failure/rejection in
// one sink never blocks or dead-letters the other.
func TestDualAnalyticsDrainer_FanOutAndIsolation(t *testing.T) {
	t.Run("both sinks receive every event", func(t *testing.T) {
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

		o := testOutbox(t)
		var wantIDs []string
		for i := 1; i <= 3; i++ {
			ev := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, fmt.Sprintf("cmp1:dual-%d", i), int64(i)), AppendWritten)
			wantIDs = append(wantIDs, ev.EventID)
		}

		gcp := &capture{}
		sink := NewHubUsageSink(o, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-dual",
		})
		d := NewDualAnalyticsDrainer(o, gcp.send, sink, trustedHub("hub-1"))

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
		for i, want := range wantIDs {
			if got := decodeUsage(t, gcp.lines[i]).EventID; got != want {
				t.Errorf("gcp[%d] = %q, want %q", i, got, want)
			}
			if gotHub[i] != want {
				t.Errorf("hub[%d] = %q, want %q", i, gotHub[i], want)
			}
		}
		if o.Stats().PendingBytes != 0 {
			t.Errorf("PendingBytes = %d after both sinks drained, want 0", o.Stats().PendingBytes)
		}
	})

	t.Run("GCP failure does not block Hub and ready file stays until GCP recovers", func(t *testing.T) {
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

		o := testOutbox(t)
		mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:gcp-fail-1", 1), AppendWritten)
		mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:gcp-fail-2", 2), AppendWritten)

		gcp := &capture{fail: errors.New("cloud logging down")}
		sink := NewHubUsageSink(o, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-dual-2",
		})
		d := NewDualAnalyticsDrainer(o, gcp.send, sink, trustedHub("hub-1"))
		clock := time.Date(2026, 11, 25, 19, 0, 0, 0, time.UTC)
		d.now = func() time.Time { return clock }

		// First tick: GCP fails, Hub still delivers both events.
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
		if o.Stats().PendingBytes == 0 {
			t.Fatal("PendingBytes must remain non-zero while GCP cursor is behind")
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
		if o.Stats().PendingBytes != 0 {
			t.Errorf("PendingBytes = %d after both cursors caught up, want 0", o.Stats().PendingBytes)
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
			// Reject index 0 permanently on Hub.
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

		o := testOutbox(t)
		mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:hub-fail-1", 1), AppendWritten)
		mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:hub-fail-2", 2), AppendWritten)

		gcp := &capture{}
		sink := NewHubUsageSink(o, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-dual-3",
		})
		d := NewDualAnalyticsDrainer(o, gcp.send, sink, trustedHub("hub-1"))
		clock := time.Date(2026, 11, 25, 19, 10, 0, 0, time.UTC)
		d.now = func() time.Time { return clock }

		// First tick: Hub is 503, GCP still receives both events cleanly.
		d.RunOnce(context.Background())
		if len(gcp.lines) != 2 {
			t.Fatalf("gcp delivered %d events while Hub was down, want 2", len(gcp.lines))
		}
		if o.Stats().PendingBytes == 0 {
			t.Fatal("PendingBytes must remain non-zero while Hub cursor is behind")
		}

		// Hub recovers and rejects event 0: Hub advances past it, ready file cleaned up, GCP already got both.
		hubFail.Store(false)
		clock = clock.Add(5 * time.Second)
		d.RunOnce(context.Background())
		if len(gcp.lines) != 2 {
			t.Errorf("gcp lines = %d, want 2", len(gcp.lines))
		}
		if o.Stats().PendingBytes != 0 {
			t.Errorf("PendingBytes = %d after both cursors finished, want 0", o.Stats().PendingBytes)
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

	t.Run("proactive re-encoded JSON expansion bisects before POST", func(t *testing.T) {
		const maxBytes = 1500
		var mu sync.Mutex
		var maxSeenBody int
		var delivered int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var req telemetrycontract.UsageIngestRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			if len(raw) > maxSeenBody {
				maxSeenBody = len(raw)
			}
			delivered += len(req.Events)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
		}))
		defer srv.Close()

		o := testOutbox(t)
		for i := 1; i <= 2; i++ {
			ev := draftUsage(telemetrycontract.SourceIdentityComposite, fmt.Sprintf("cmp1:esc-%d", i), int64(i))
			// '<' expands from 1 byte to 6 bytes (\u003c) under encoding/json HTML escaping.
			ev.SessionID = strings.Repeat("<", 80)
			mustAppend(t, o, ev, AppendWritten)
		}

		sink := NewHubUsageSink(o, HubUsageSinkConfig{
			Post:           httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID:          "run-esc",
			MaxBatchEvents: 500,
			MaxBatchBytes:  maxBytes,
		})
		d := NewHubAnalyticsDrainer(o, sink, trustedHub("hub-1"))

		if sent := d.RunOnce(context.Background()); sent != 2 {
			t.Fatalf("sent = %d, want 2", sent)
		}
		mu.Lock()
		defer mu.Unlock()
		if delivered != 2 {
			t.Fatalf("delivered = %d, want 2", delivered)
		}
		if maxSeenBody > maxBytes {
			t.Errorf("max HTTP body size = %d > maxBytes %d (proactive re-encoded bisect failed)", maxSeenBody, maxBytes)
		}
	})
}

// R1(a) & R1(b): in dual-run mode at a small outbox cap, a persistently failing
// Hub sink (404 capability revoked OR 500 outage) must never cause the healthy
// GCP Cloud Logging export to overflow or drop events.
func TestDualAnalyticsDrainer_FailingHubNeverOverflowsGCP(t *testing.T) {
	t.Run("404 capability revoked stops Hub cursor and GCP receives 200/200", func(t *testing.T) {
		var hubCalls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hubCalls.Add(1)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("not found"))
		}))
		defer srv.Close()

		o := testOutbox(t)
		o.maxPending = 16 << 10 // 16 KiB cap (reviewer's exact probe)
		gcp := &capture{}
		sink := NewHubUsageSink(o, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-404",
		})
		d := NewDualAnalyticsDrainer(o, gcp.send, sink, trustedHub("hub-1"))

		for i := 1; i <= 200; i++ {
			mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, fmt.Sprintf("cmp1:rev-%d", i), int64(i)), AppendWritten)
			d.RunOnce(context.Background())
		}

		if !d.HubRevoked() {
			t.Fatal("expected HubRevoked() = true after HTTP 404")
		}
		if got := hubCalls.Load(); got != 1 {
			t.Errorf("hubCalls = %d, want 1 (must stop after first 404, not retry forever)", got)
		}
		if len(gcp.lines) != 200 {
			t.Errorf("gcp received %d/200 events, want 200/200", len(gcp.lines))
		}
		stats := o.Stats()
		if stats.Overflow != 0 || stats.PersistedOverflow != 0 {
			t.Errorf("overflow = %d (persisted %d), want 0", stats.Overflow, stats.PersistedOverflow)
		}
		if stats.PendingBytes != 0 {
			t.Errorf("PendingBytes = %d, want 0 after Hub cursor unregistered", stats.PendingBytes)
		}
	})

	t.Run("Hub-only 404 stops draining rather than retrying forever", func(t *testing.T) {
		var hubCalls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hubCalls.Add(1)
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		o := testOutbox(t)
		mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:hubonly-404", 1), AppendWritten)
		sink := NewHubUsageSink(o, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-hubonly-404",
		})
		d := NewHubAnalyticsDrainer(o, sink, trustedHub("hub-1"))
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
		if got := hubCalls.Load(); got != 1 {
			t.Errorf("hubCalls = %d, want 1 (must not retry forever)", got)
		}
	})

	t.Run("persistent 500 Hub reclaims Hub lag and GCP receives 200/200 with recorded gap", func(t *testing.T) {
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

		o := testOutbox(t)
		o.maxPending = 16 << 10 // 16 KiB cap (reviewer's exact probe)
		gcp := &capture{}
		sink := NewHubUsageSink(o, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-500",
		})
		d := NewDualAnalyticsDrainer(o, gcp.send, sink, trustedHub("hub-1"))
		clock := time.Date(2026, 11, 25, 20, 0, 0, 0, time.UTC)
		d.now = func() time.Time { return clock }

		for i := 1; i <= 200; i++ {
			mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, fmt.Sprintf("cmp1:500-%d", i), int64(i)), AppendWritten)
			d.RunOnce(context.Background())
		}

		if len(gcp.lines) != 200 {
			t.Fatalf("gcp received %d/200 events while Hub returned 500, want 200/200", len(gcp.lines))
		}
		stats := o.Stats()
		if stats.Overflow != 0 || stats.PersistedOverflow != 0 {
			t.Errorf("overflow = %d (persisted %d), want 0", stats.Overflow, stats.PersistedOverflow)
		}

		// When Hub recovers, client does NOT send MissingRanges (Hub is sole
		// authority for meter_health gaps via emitted-vs-received sequences),
		// and delivered events begin past seq 1 after reclamation.
		hubDown.Store(false)
		clock = clock.Add(10 * time.Minute)
		d.RunOnce(context.Background())

		mu.Lock()
		defer mu.Unlock()
		if len(recoveredReqs) == 0 {
			t.Fatal("expected Hub request after recovery")
		}
		var minDeliveredSeq int64
		for _, req := range recoveredReqs {
			if len(req.MissingRanges) != 0 {
				t.Errorf("client must not send MissingRanges (Hub is sole gap authority), got %+v", req.MissingRanges)
			}
			for _, ev := range req.Events {
				if minDeliveredSeq == 0 || ev.ProducerSeq < minDeliveredSeq {
					minDeliveredSeq = ev.ProducerSeq
				}
			}
		}
		if minDeliveredSeq <= 1 {
			t.Errorf("minDeliveredSeq = %d, want > 1 after lag reclamation", minDeliveredSeq)
		}
	})
}

// RV1 concurrency test (-race): reclaimSecondaryLagLocked running concurrently
// under the outbox lock must never advance cursor.hub past lines that
// DrainBatchCursor currently has in flight, and advanceCursorNamed must never
// race with reclaim.
func TestDualAnalyticsDrainer_ReclaimDuringInFlightDrainRace(t *testing.T) {
	inFlightStarted := make(chan struct{})
	releaseHub := make(chan struct{})
	var startOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseHub) }) }
	t.Cleanup(release)
	var mu sync.Mutex
	var deliveredSeqs []int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(req.Events) > 0 {
			startOnce.Do(func() {
				close(inFlightStarted)
				<-releaseHub
			})
			mu.Lock()
			for _, ev := range req.Events {
				deliveredSeqs = append(deliveredSeqs, ev.ProducerSeq)
			}
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
	}))
	defer srv.Close()

	o := testOutbox(t)
	o.maxPending = 8 << 10 // 8 KiB cap to force reclaim attempts
	o.lockWait = contentionLockWait
	gcp := &capture{}
	sink := NewHubUsageSink(o, HubUsageSinkConfig{
		Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
		RunID: "run-race",
	})
	d := NewDualAnalyticsDrainer(o, gcp.send, sink, trustedHub("hub-1"))

	// Seed ready-1.jsonl with 2 events and let GCP drain it (primFile = 2)
	// while keeping Hub blocked mid-POST on ready-1.jsonl.
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:race-1", 1), AppendWritten)
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:race-2", 2), AppendWritten)

	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		d.RunOnce(context.Background())
	}()

	<-inFlightStarted

	// While DrainBatchCursor has ready-1.jsonl in flight, concurrently hammer
	// AppendUsage to push retainedBytes past maxPending and trigger reclaim.
	appendDone := make(chan struct{})
	go func() {
		defer close(appendDone)
		for i := 3; i <= 40; i++ {
			_, _ = o.AppendUsage(draftUsage(telemetrycontract.SourceIdentityComposite, fmt.Sprintf("cmp1:race-%d", i), int64(i)))
		}
	}()
	<-appendDone

	// Verify cursor.hub was NOT advanced past ready-1.jsonl while it was in flight.
	hubFile, _, err := o.readCursorNamed(HubAnalyticsCursorName)
	if err != nil {
		t.Fatalf("readCursorNamed: %v", err)
	}
	if hubFile > 1 {
		t.Fatalf("cursor.hub advanced to file %d while ready-1.jsonl was still in flight", hubFile)
	}
	if _, err := os.Stat(filepath.Join(o.Dir(), readyName(1))); err != nil {
		t.Fatalf("ready-1.jsonl was deleted while still in flight: %v", err)
	}

	// Release the in-flight Hub POST and verify clean completion with no race.
	release()
	<-drainDone

	hubFileAfter, _, err := o.readCursorNamed(HubAnalyticsCursorName)
	if err != nil {
		t.Fatalf("readCursorNamed after drain: %v", err)
	}
	if hubFileAfter < 2 {
		t.Errorf("cursor.hub = %d after ready-1.jsonl ACK, want >= 2", hubFileAfter)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(deliveredSeqs) < 2 || deliveredSeqs[0] != 1 || deliveredSeqs[1] != 2 {
		t.Errorf("deliveredSeqs = %v, want [1, 2, ...]", deliveredSeqs)
	}
}

// R2: cursor registration is persisted in cursors.json so a second
// OpenAnalyticsOutbox handle (simulating a hook subprocess) computes the same
// activeCursors() and retainedBytes(), enforcing maxPending via dual-run lag
// reclamation instead of letting ready files grow unbounded.
func TestDualAnalyticsDrainer_SecondHandleReclaimsLagAndRespectsMaxPending(t *testing.T) {
	t.Run("second handle appends under failing Hub stay within maxPending", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("hub down"))
		}))
		defer srv.Close()

		dir := filepath.Join(t.TempDir(), "outbox")
		oDrainer, err := OpenAnalyticsOutbox(dir)
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox(drainer): %v", err)
		}
		oDrainer.maxPending = 16 << 10

		gcp := &capture{}
		sink := NewHubUsageSink(oDrainer, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-r2",
		})
		d := NewDualAnalyticsDrainer(oDrainer, gcp.send, sink, trustedHub("hub-1"))

		// Second independent handle simulating short-lived `sciontool hook` subprocesses.
		oHook, err := OpenAnalyticsOutbox(dir)
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox(hook): %v", err)
		}
		oHook.maxPending = 16 << 10

		for i := 1; i <= 200; i++ {
			mustAppend(t, oHook, draftUsage(telemetrycontract.SourceIdentityComposite, fmt.Sprintf("cmp1:hook-%d", i), int64(i)), AppendWritten)
			d.RunOnce(context.Background())
		}

		if len(gcp.lines) != 200 {
			t.Fatalf("gcp received %d/200 events, want 200/200", len(gcp.lines))
		}
		stats := oHook.Stats()
		if stats.Overflow != 0 || stats.PersistedOverflow != 0 {
			t.Errorf("overflow = %d (persisted %d), want 0", stats.Overflow, stats.PersistedOverflow)
		}
		if stats.RetainedBytes > 16<<10 {
			t.Errorf("RetainedBytes = %d > maxPending %d (second handle failed to reclaim lagging Hub cursor)", stats.RetainedBytes, 16<<10)
		}
		if _, err := os.Stat(filepath.Join(dir, analyticsCursorsFile)); err != nil {
			t.Errorf("expected %s on disk: %v", analyticsCursorsFile, err)
		}
	})

	t.Run("Hub-only mode with stale legacy cursor file uses cursor.hub on second handle", func(t *testing.T) {
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

		dir := filepath.Join(t.TempDir(), "outbox")
		oDrainer, err := OpenAnalyticsOutbox(dir)
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox(drainer): %v", err)
		}
		// Seed a stale legacy "cursor" file at (1, 0) on disk.
		if err := oDrainer.writeCursorNamed("cursor", 1, 0); err != nil {
			t.Fatalf("write legacy cursor: %v", err)
		}

		sink := NewHubUsageSink(oDrainer, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-r2-hubonly",
		})
		d := NewHubAnalyticsDrainer(oDrainer, sink, trustedHub("hub-1"))

		// Second handle opens the same outbox and must read cursors.json -> ["cursor.hub"].
		oHook, err := OpenAnalyticsOutbox(dir)
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox(hook): %v", err)
		}
		if got := oHook.activeCursors(); len(got) != 1 || got[0] != HubAnalyticsCursorName {
			t.Fatalf("second handle activeCursors() = %v, want [%q]", got, HubAnalyticsCursorName)
		}
		mustAppend(t, oHook, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:hubonly-stale-1", 10), AppendWritten)
		d.RunOnce(context.Background())

		if got := oHook.Stats().PendingBytes; got != 0 {
			t.Errorf("second handle PendingBytes = %d, want 0", got)
		}
		mu.Lock()
		defer mu.Unlock()
		if delivered != 1 {
			t.Errorf("delivered = %d, want 1", delivered)
		}
	})
}

// O1: periodic re-probe from a GCP-only drainer activates the Hub sink in
// dual-run mode (with cursor.hub) when capabilities turn positive, without
// disrupting the GCP drain.
func TestDualAnalyticsDrainer_PeriodicReprobeActivatesHubSink(t *testing.T) {
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

	o := testOutbox(t)
	gcp := &capture{}
	d := NewAnalyticsDrainer(o, gcp.send, trustedHub("hub-1"))
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
	}, 60*time.Second, nil, nil)

	// Tick 1 (t=0): probe #1 returns 503; GCP still delivers event 1.
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:rp-1", 1), AppendWritten)
	d.RunOnce(context.Background())
	if len(gcp.lines) != 1 {
		t.Fatalf("gcp lines = %d, want 1", len(gcp.lines))
	}

	// Tick 2 (t+60s): probe #2 returns 200; Hub sink activates in dual-run mode
	// and both GCP and Hub receive event 2.
	clock = clock.Add(60 * time.Second)
	ev2 := mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:rp-2", 2), AppendWritten)
	d.RunOnce(context.Background())

	if len(gcp.lines) != 2 {
		t.Fatalf("gcp lines = %d, want 2", len(gcp.lines))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hubDelivered) != 1 || hubDelivered[0] != ev2.EventID {
		t.Errorf("hubDelivered = %v, want [%q]", hubDelivered, ev2.EventID)
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

// O4: Hub sink always uses cursor.hub in both dual-run and Hub-only modes.
func TestHubUsageSink_AlwaysUsesCursorHub(t *testing.T) {
	t.Run("dual-run to Hub-only restart resumes from cursor.hub without skipping", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "outbox")
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

		// Process 1: dual-run; GCP drains all 3 events, Hub fails so cursor.hub stays behind.
		o1, err := OpenAnalyticsOutbox(dir)
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox(1): %v", err)
		}
		var wantIDs []string
		for i := 1; i <= 3; i++ {
			ev := mustAppend(t, o1, draftUsage(telemetrycontract.SourceIdentityComposite, fmt.Sprintf("cmp1:o4-%d", i), int64(i)), AppendWritten)
			wantIDs = append(wantIDs, ev.EventID)
		}
		gcp := &capture{}
		sink1 := NewHubUsageSink(o1, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-o4",
		})
		d1 := NewDualAnalyticsDrainer(o1, gcp.send, sink1, trustedHub("hub-1"))
		d1.RunOnce(context.Background())
		if len(gcp.lines) != 3 {
			t.Fatalf("gcp received %d, want 3", len(gcp.lines))
		}

		// Process 2: restarts in Hub-only mode (GCP config removed); Hub recovers.
		hubDown.Store(false)
		o2, err := OpenAnalyticsOutbox(dir)
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox(2): %v", err)
		}
		sink2 := NewHubUsageSink(o2, HubUsageSinkConfig{
			Post:  httptestPostFunc(srv.URL, "agent-1", "tok-1"),
			RunID: "run-o4",
		})
		d2 := NewHubAnalyticsDrainer(o2, sink2, trustedHub("hub-1"))
		if sent := d2.RunOnce(context.Background()); sent != 3 {
			t.Fatalf("Hub-only restart sent %d, want 3 (must resume from cursor.hub, not GCP cursor)", sent)
		}
		if _, err := os.Stat(filepath.Join(dir, HubAnalyticsCursorName)); err != nil {
			t.Errorf("expected %s file to exist: %v", HubAnalyticsCursorName, err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(hubIDs) != 3 {
			t.Fatalf("hubIDs = %v, want 3 events", hubIDs)
		}
		for i, want := range wantIDs {
			if hubIDs[i] != want {
				t.Errorf("hubIDs[%d] = %q, want %q", i, hubIDs[i], want)
			}
		}
	})
}
