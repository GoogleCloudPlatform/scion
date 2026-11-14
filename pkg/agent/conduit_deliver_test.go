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

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// fakeDeliverer is a ConduitDeliverer answering from a script; the last
// answer repeats.
type fakeDeliverer struct {
	mu      sync.Mutex
	answers []fakeAnswer
	reqs    []agentcontrol.DeliverRequest
	onCall  func()
}

type fakeAnswer struct {
	status int
	resp   agentcontrol.DeliverResponse
	err    error
}

func (f *fakeDeliverer) DeliverMessage(_ context.Context, agentID string, req *agentcontrol.DeliverRequest) (int, agentcontrol.DeliverResponse, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, *req)
	a := f.answers[min(len(f.reqs), len(f.answers))-1]
	cb := f.onCall
	f.mu.Unlock()
	if cb != nil {
		cb()
	}
	return a.status, a.resp, a.err
}

func (f *fakeDeliverer) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func answer(status int, outcome string) fakeAnswer {
	return fakeAnswer{status: status, resp: agentcontrol.DeliverResponse{Outcome: outcome}}
}

// newConduitTestManager returns a manager on a runtime that delivers over
// conduit, whose single agent carries the launch label when labelled, and
// which records every exec into *execs.
func newConduitTestManager(t *testing.T, labelled bool, spec api.DeliverySpec, execs *[]string) *AgentManager {
	t.Helper()
	orig := conduitDeliverRuntimes
	conduitDeliverRuntimes = map[string]bool{"mock": true}
	origBackoff := conduitRetryBackoff
	conduitRetryBackoff = time.Millisecond
	t.Cleanup(func() { conduitDeliverRuntimes, conduitRetryBackoff = orig, origBackoff })
	labels := map[string]string{"scion.name": "test-agent"}
	if labelled {
		labels[LabelConduitDeliver] = "true"
	}
	var mu sync.Mutex
	rt := &runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{ID: "hub-agent-1", ContainerID: "c-1", Name: "test-agent", Labels: labels}}, nil
		},
		ExecFunc: func(_ context.Context, _ string, cmd []string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			*execs = append(*execs, strings.Join(cmd, " "))
			return "", nil
		},
		ExecWithStdinFunc: func(_ context.Context, _ string, cmd []string, _ io.Reader) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			*execs = append(*execs, strings.Join(cmd, " "))
			return "", nil
		},
	}
	return &AgentManager{
		Runtime:    rt,
		newHarness: func(string) api.Harness { return &deliveryTestHarness{spec: spec} },
	}
}

var shippedSpec = api.DeliverySpec{SubmitKeys: []string{"Enter"}, ExtraSubmits: 2, ExtraSubmitDelay: time.Millisecond}

func TestLaunchedWithConduitDeliver(t *testing.T) {
	for _, tc := range []struct {
		env  []string
		want bool
	}{
		{[]string{"SCION_HUB_EXPERIMENTS=hub.conduit,hub.conduit_deliver"}, true},
		{[]string{"A=b", "SCION_HUB_EXPERIMENTS= hub.conduit_deliver"}, true},
		{[]string{"SCION_HUB_EXPERIMENTS=hub.conduit"}, false},
		{[]string{"OTHER=hub.conduit_deliver"}, false},
		{nil, false},
	} {
		if got := launchedWithConduitDeliver(tc.env); got != tc.want {
			t.Errorf("launchedWithConduitDeliver(%q) = %t, want %t", tc.env, got, tc.want)
		}
	}
	if !agentLaunchedWithConduitDeliver(&api.AgentInfo{Labels: map[string]string{labelConduitDeliverGCP: "true"}}) {
		t.Error("the Cloud Run sanitised label is not recognised")
	}
}

// TestConduitDelivery_Delivered: a labelled agent with message ids is
// delivered over conduit, with no exec at all; the request carries the
// fixed schema built from the harness settings.
func TestConduitDelivery_Delivered(t *testing.T) {
	var execs []string
	m := newConduitTestManager(t, true, shippedSpec, &execs)
	d := &fakeDeliverer{answers: []fakeAnswer{answer(200, agentcontrol.OutcomeDelivered)}}
	m.SetConduitDeliverer(d)
	err := m.deliver(context.Background(), deliveryRequest{agentID: "test-agent", message: "hello", messageIDs: []string{"m1", "m2"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(execs) != 0 {
		t.Fatalf("exec used: %v", execs)
	}
	if d.calls() != 1 {
		t.Fatalf("%d deliverer calls", d.calls())
	}
	r := d.reqs[0]
	if r.Op != agentcontrol.OpPaste || *r.Text != "hello" || r.ExtraSubmits.Count != 2 || r.Ready != nil ||
		len(r.MessageIDs) != 2 || r.SubmitKeys[0] != "Enter" || r.DeadlineMS != 92000 || r.BrokerID != "" {
		t.Fatalf("request = %+v", r)
	}
	if err := r.ValidateFromBroker(); err != nil {
		t.Fatalf("request does not validate: %v", err)
	}
}

// TestConduitDelivery_ExistingPathUnlessRouted (A2, A3): no deliverer (the
// broker never set one), an agent without the launch label, a runtime that
// does not deliver over conduit, or a delivery without hub ids all keep the
// exec path and never call the hub.
func TestConduitDelivery_ExistingPathUnlessRouted(t *testing.T) {
	cases := map[string]func(t *testing.T) (*AgentManager, *fakeDeliverer, *[]string, deliveryRequest){
		"no deliverer": func(t *testing.T) (*AgentManager, *fakeDeliverer, *[]string, deliveryRequest) {
			var execs []string
			m := newConduitTestManager(t, true, shippedSpec, &execs)
			return m, nil, &execs, deliveryRequest{agentID: "test-agent", message: "x", messageIDs: []string{"m"}}
		},
		"agent launched without the flag": func(t *testing.T) (*AgentManager, *fakeDeliverer, *[]string, deliveryRequest) {
			var execs []string
			m := newConduitTestManager(t, false, shippedSpec, &execs)
			return m, &fakeDeliverer{}, &execs, deliveryRequest{agentID: "test-agent", message: "x", messageIDs: []string{"m"}}
		},
		"docker runtime": func(t *testing.T) (*AgentManager, *fakeDeliverer, *[]string, deliveryRequest) {
			var execs []string
			m := newConduitTestManager(t, true, shippedSpec, &execs)
			conduitDeliverRuntimes = map[string]bool{"cloudrun": true}
			return m, &fakeDeliverer{}, &execs, deliveryRequest{agentID: "test-agent", message: "x", messageIDs: []string{"m"}}
		},
		"no message ids": func(t *testing.T) (*AgentManager, *fakeDeliverer, *[]string, deliveryRequest) {
			var execs []string
			m := newConduitTestManager(t, true, shippedSpec, &execs)
			return m, &fakeDeliverer{}, &execs, deliveryRequest{agentID: "test-agent", message: "x"}
		},
		"a message without an id": func(t *testing.T) (*AgentManager, *fakeDeliverer, *[]string, deliveryRequest) {
			var execs []string
			m := newConduitTestManager(t, true, shippedSpec, &execs)
			return m, &fakeDeliverer{}, &execs, deliveryRequest{agentID: "test-agent", message: "x", messageIDs: []string{"m"}, partialIDs: true}
		},
		"settings outside the route bounds": func(t *testing.T) (*AgentManager, *fakeDeliverer, *[]string, deliveryRequest) {
			var execs []string
			m := newConduitTestManager(t, true, api.DeliverySpec{SubmitKeys: []string{"Enter"}, ExtraSubmits: 4}, &execs)
			return m, &fakeDeliverer{}, &execs, deliveryRequest{agentID: "test-agent", message: "x", messageIDs: []string{"m"}}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			m, d, execs, req := setup(t)
			if d != nil {
				d.answers = []fakeAnswer{answer(200, agentcontrol.OutcomeDelivered)}
				m.SetConduitDeliverer(d)
			}
			if err := m.deliver(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if d != nil && d.calls() != 0 {
				t.Fatalf("hub called %d times", d.calls())
			}
			if !execDelivered(*execs) {
				t.Fatalf("exec path not used: %v", *execs)
			}
		})
	}
}

// TestConduitDelivery_AnswerMatrix: the retry matrix of design v2.22 §2.4.
func TestConduitDelivery_AnswerMatrix(t *testing.T) {
	cases := []struct {
		name      string
		answers   []fakeAnswer
		calls     int
		exec      bool
		partial   bool
		final     bool
		succeeded bool
	}{
		{"partial", []fakeAnswer{answer(200, agentcontrol.OutcomePartial)}, 1, false, true, false, false},
		{"in progress", []fakeAnswer{answer(409, agentcontrol.OutcomeInProgress)}, 1, false, true, false, false},
		{"ambiguous transport error", []fakeAnswer{{err: errors.New("reset after send")}}, 1, false, true, false, false},
		{"hub timeout after send", []fakeAnswer{answer(504, "unknown")}, 1, false, true, false, false},
		{"bound", []fakeAnswer{answer(409, agentcontrol.OutcomeBound)}, 1, false, false, true, false},
		{"expired before paste", []fakeAnswer{answer(408, agentcontrol.OutcomeExpiredBeforePaste)}, 1, false, false, true, false},
		{"invalid", []fakeAnswer{answer(400, agentcontrol.OutcomeInvalid)}, 1, false, false, true, false},
		{"experiment off at the hub (404)", []fakeAnswer{{status: 404}}, 1, true, false, false, true},
		{"too large (413)", []fakeAnswer{{status: 413}}, 1, true, false, false, true},
		{"unavailable", []fakeAnswer{{err: ErrConduitUnavailable}}, 1, true, false, false, true},
		{"not reached x3 then exec", []fakeAnswer{answer(503, "not_reached")}, 3, true, false, false, true},
		{"busy then delivered", []fakeAnswer{answer(409, agentcontrol.OutcomeBusy), answer(200, agentcontrol.OutcomeDelivered)}, 2, false, false, false, true},
		{"not sent then delivered", []fakeAnswer{{err: ErrConduitNotSent}, answer(200, agentcontrol.OutcomeDelivered)}, 2, false, false, false, true},
		{"422 x3 then exec", []fakeAnswer{answer(422, agentcontrol.OutcomeNotDelivered)}, 3, true, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var execs []string
			m := newConduitTestManager(t, true, shippedSpec, &execs)
			d := &fakeDeliverer{answers: tc.answers}
			m.SetConduitDeliverer(d)
			err := m.deliver(context.Background(), deliveryRequest{agentID: "test-agent", message: "x", messageIDs: []string{"m"}})
			if d.calls() != tc.calls {
				t.Errorf("hub calls = %d, want %d", d.calls(), tc.calls)
			}
			ids := map[string]bool{}
			for _, r := range d.reqs {
				ids[r.DeliveryID] = true
			}
			if len(ids) > 1 {
				t.Errorf("retries used %d delivery ids, want one", len(ids))
			}
			if got := len(execs) > 0; got != tc.exec {
				t.Errorf("exec used = %t (%v), want %t", got, execs, tc.exec)
			}
			var partial *PartialDeliveryError
			var final *finalDeliveryError
			if got := errors.As(err, &partial); got != tc.partial {
				t.Errorf("partial = %t (err %v), want %t", got, err, tc.partial)
			}
			if got := errors.As(err, &final); got != tc.final {
				t.Errorf("final = %t (err %v), want %t", got, err, tc.final)
			}
			if (err == nil) != tc.succeeded {
				t.Errorf("err = %v, want success %t", err, tc.succeeded)
			}
		})
	}
}

// TestConduitDelivery_ExecFallbackFailureIsFinal: when exec fails after a
// conduit fallback, the error says why conduit was not used and is not
// retried by the buffer (which would run both paths again).
func TestConduitDelivery_ExecFallbackFailureIsFinal(t *testing.T) {
	var execs []string
	m := newConduitTestManager(t, true, shippedSpec, &execs)
	m.Runtime.(*runtime.MockRuntime).ExecWithStdinFunc = func(context.Context, string, []string, io.Reader) (string, error) {
		return "", errors.New("no exec connector")
	}
	m.SetConduitDeliverer(&fakeDeliverer{answers: []fakeAnswer{{status: http.StatusNotFound}}})
	err := m.deliver(context.Background(), deliveryRequest{agentID: "test-agent", message: "x", messageIDs: []string{"m"}})
	var final *finalDeliveryError
	if !errors.As(err, &final) || !strings.Contains(err.Error(), "conduit not used") {
		t.Fatalf("err = %v", err)
	}
}

// TestConduitDelivery_Interrupt: an interrupt carries its one message id
// and the harness's interrupt keys, with no readiness block.
func TestConduitDelivery_Interrupt(t *testing.T) {
	var execs []string
	m := newConduitTestManager(t, true, shippedSpec, &execs)
	d := &fakeDeliverer{answers: []fakeAnswer{answer(200, agentcontrol.OutcomeDelivered)}}
	m.SetConduitDeliverer(d)
	ctx := WithDeliveryMessageID(context.Background(), "m-int")
	if err := m.Message(ctx, "test-agent", "", "stop", true); err != nil {
		t.Fatal(err)
	}
	if d.calls() != 1 {
		t.Fatalf("%d calls", d.calls())
	}
	r := d.reqs[0]
	if r.Op != agentcontrol.OpInterruptPaste || len(r.InterruptKeys) == 0 || r.Ready != nil || r.MessageIDs[0] != "m-int" {
		t.Fatalf("request = %+v", r)
	}
	// An empty interrupt is interrupt_submit with no text.
	if err := m.Message(ctx, "test-agent", "", "", true); err != nil {
		t.Fatal(err)
	}
	if r := d.reqs[1]; r.Op != agentcontrol.OpInterruptSubmit || r.Text != nil || r.ExtraSubmits != nil {
		t.Fatalf("empty interrupt = %+v", r)
	}
}

// TestConduitDelivery_LockHeldAcrossRPC: the per-container injection lock
// is held while the hub call runs.
func TestConduitDelivery_LockHeldAcrossRPC(t *testing.T) {
	var execs []string
	m := newConduitTestManager(t, true, shippedSpec, &execs)
	held := false
	d := &fakeDeliverer{answers: []fakeAnswer{answer(200, agentcontrol.OutcomeDelivered)}}
	d.onCall = func() { held = len(m.injectionLock("c-1").ch) == 0 }
	m.SetConduitDeliverer(d)
	if err := m.deliver(context.Background(), deliveryRequest{agentID: "test-agent", message: "x", messageIDs: []string{"m"}}); err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("injection lock not held during the hub call")
	}
}

// TestConduitDelivery_BufferedFlush: a buffered message with a hub id
// flushes over conduit: the broker skips its own readiness probe (no exec)
// and asks sciontool to wait instead.
func TestConduitDelivery_BufferedFlush(t *testing.T) {
	var execs []string
	m := newConduitTestManager(t, true, shippedSpec, &execs)
	d := &fakeDeliverer{answers: []fakeAnswer{answer(200, agentcontrol.OutcomeDelivered)}}
	m.SetConduitDeliverer(d)
	done := make(chan struct{})
	m.msgBuffer = NewMessageBuffer(10*time.Millisecond, func(string, string, string, bool) error {
		t.Error("deliverFunc used instead of deliverFlush")
		return nil
	})
	m.msgBuffer.waitReady = m.waitHarnessReady
	m.msgBuffer.deliverFlush = func(f bufferedFlush) error {
		defer close(done)
		return m.deliver(context.Background(), deliveryRequest{agentID: f.agentID, projectID: f.projectID,
			message: f.text, messageIDs: f.messageIDs, partialIDs: f.partialIDs, readyWanted: f.readyWanted})
	}
	defer m.msgBuffer.Close()
	if err := m.Message(WithDeliveryMessageID(context.Background(), "m-1"), "test-agent", "", "hello", false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("no flush")
	}
	if len(execs) != 0 {
		t.Fatalf("exec used (readiness probe or delivery): %v", execs)
	}
	r := d.reqs[0]
	want := effectiveReadySpec(&deliveryTestHarness{spec: shippedSpec})
	if r.Ready == nil || r.Ready.Strategy != want.Strategy || r.Ready.SettleMS != int(want.Settle.Milliseconds()) || r.MessageIDs[0] != "m-1" {
		t.Fatalf("request = %+v ready %+v", r, r.Ready)
	}
}

// TestDeliverImmediate_SanitisesOnExecPath: the exec path now uses the
// shared steps, so text is sanitised there too (design v2.22 §5.3): a
// pasted paste-end marker cannot end the bracketed paste.
func TestDeliverImmediate_SanitisesOnExecPath(t *testing.T) {
	var stdin string
	mgr := newDeliveryTestManager(shippedSpec, new([]string))
	mgr.Runtime.(*runtime.MockRuntime).ExecWithStdinFunc = func(_ context.Context, _ string, _ []string, r io.Reader) (string, error) {
		b, _ := io.ReadAll(r)
		stdin = string(b)
		return "", nil
	}
	if err := mgr.deliverImmediate(context.Background(), "test-agent", "", "a\x1b[201~b\u009bc\r", false); err != nil {
		t.Fatal(err)
	}
	if stdin != "a[201~bc" {
		t.Fatalf("stdin = %q, want the sanitised text", stdin)
	}
}

// TestConduitDelivery_LogsNoText: the broker's conduit delivery lines carry
// ids, counts and outcomes, never the text or key names.
func TestConduitDelivery_LogsNoText(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	var execs []string
	m := newConduitTestManager(t, true, shippedSpec, &execs)
	m.SetConduitDeliverer(&fakeDeliverer{answers: []fakeAnswer{{status: 404}}})
	if err := m.deliver(context.Background(), deliveryRequest{agentID: "test-agent", message: "SENTINEL-BROKER-TEXT", messageIDs: []string{"m"}}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "message delivery over conduit") {
		t.Fatalf("no conduit delivery line: %q", out)
	}
	for _, bad := range []string{"SENTINEL-BROKER-TEXT", "Enter"} {
		if strings.Contains(out, bad) {
			t.Fatalf("log contains %q: %q", bad, out)
		}
	}
}

// TestConduitDelivery_OversizeUsesExec (review R3): a flush too large for
// the route (raw text over 512 KiB, text whose JSON encoding exceeds the
// request limit, or more than 64 coalesced messages) never goes to the hub;
// it uses exec, and when exec fails the error names the limit and is final.
func TestConduitDelivery_OversizeUsesExec(t *testing.T) {
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("m-%d", i)
		}
		return out
	}
	cases := map[string]struct {
		text  string
		ids   []string
		field string
	}{
		"text 512 KiB + 1":       {strings.Repeat("x", agentcontrol.MaxDeliverText+1), []string{"m"}, "text"},
		"encoded text too large": {strings.Repeat("<", 200*1024), []string{"m"}, "text"},
		"65 messages":            {"hello", ids(65), "message_ids"},
	}
	for name, tc := range cases {
		for _, execOK := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s exec=%t", name, execOK), func(t *testing.T) {
				var execs []string
				m := newConduitTestManager(t, true, shippedSpec, &execs)
				if !execOK {
					m.Runtime.(*runtime.MockRuntime).ExecWithStdinFunc = func(context.Context, string, []string, io.Reader) (string, error) {
						return "", errors.New("no exec connector")
					}
				}
				d := &fakeDeliverer{answers: []fakeAnswer{answer(200, agentcontrol.OutcomeDelivered)}}
				m.SetConduitDeliverer(d)
				err := m.deliver(context.Background(), deliveryRequest{agentID: "test-agent", message: tc.text, messageIDs: tc.ids})
				if d.calls() != 0 {
					t.Fatalf("hub called %d times for an oversize flush", d.calls())
				}
				if execOK {
					if err != nil || !execDelivered(execs) {
						t.Fatalf("err %v execs %v; want delivered over exec", err, execs)
					}
					return
				}
				var final *finalDeliveryError
				if !errors.As(err, &final) || !strings.Contains(err.Error(), "route limits") || !strings.Contains(err.Error(), tc.field) {
					t.Fatalf("err = %v; want a final error naming %s", err, tc.field)
				}
			})
		}
	}
	// Control: just inside the raw limit, with plain text, uses conduit.
	var execs []string
	m := newConduitTestManager(t, true, shippedSpec, &execs)
	d := &fakeDeliverer{answers: []fakeAnswer{answer(200, agentcontrol.OutcomeDelivered)}}
	m.SetConduitDeliverer(d)
	if err := m.deliver(context.Background(), deliveryRequest{agentID: "test-agent", message: strings.Repeat("x", agentcontrol.MaxDeliverText-1024), messageIDs: ids(64)}); err != nil || d.calls() != 1 {
		t.Fatalf("control: err %v calls %d", err, d.calls())
	}
}

// nopPreDelivery stands in for a message's attachment work.
type nopPreDelivery struct{}

func (nopPreDelivery) BeforeText(context.Context, runtime.Runtime, string) string { return "" }
func (nopPreDelivery) Discard()                                                   {}

// TestConduitDelivery_AttachmentsUseExec (review FYI, design v2.22 §1.6): a
// buffered flush or an interrupt carrying attachments uses exec, never the
// hub.
func TestConduitDelivery_AttachmentsUseExec(t *testing.T) {
	t.Run("interrupt", func(t *testing.T) {
		var execs []string
		m := newConduitTestManager(t, true, shippedSpec, &execs)
		d := &fakeDeliverer{answers: []fakeAnswer{answer(200, agentcontrol.OutcomeDelivered)}}
		m.SetConduitDeliverer(d)
		ctx := WithPreDelivery(WithDeliveryMessageID(context.Background(), "m-1"), nopPreDelivery{})
		if err := m.Message(ctx, "test-agent", "", "with file", true); err != nil {
			t.Fatal(err)
		}
		if d.calls() != 0 || len(execs) == 0 {
			t.Fatalf("hub calls %d, execs %v; want exec only", d.calls(), execs)
		}
	})
	t.Run("buffered", func(t *testing.T) {
		var execs []string
		m := newConduitTestManager(t, true, shippedSpec, &execs)
		d := &fakeDeliverer{answers: []fakeAnswer{answer(200, agentcontrol.OutcomeDelivered)}}
		m.SetConduitDeliverer(d)
		done := make(chan struct{})
		m.msgBuffer = NewMessageBuffer(10*time.Millisecond, func(string, string, string, bool) error { return nil })
		m.msgBuffer.preDeliver = func(context.Context, string, string, PreDelivery) string { return "" }
		m.msgBuffer.deliverFlush = func(f bufferedFlush) error {
			defer close(done)
			if !f.hasAttachments {
				t.Error("flush lost the attachment flag")
			}
			return m.deliver(context.Background(), deliveryRequest{agentID: f.agentID, message: f.text,
				messageIDs: f.messageIDs, hasAttachments: f.hasAttachments})
		}
		defer m.msgBuffer.Close()
		m.msgBuffer.sendWithID("test-agent", "", "plain", "m-1", nil, nil)
		m.msgBuffer.sendWithID("test-agent", "", "with file", "m-2", nil, nopPreDelivery{})
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("no flush")
		}
		if d.calls() != 0 || len(execs) == 0 {
			t.Fatalf("hub calls %d, execs %v; want exec only", d.calls(), execs)
		}
	})
}

// execDelivered reports whether execs hold an exec delivery (a load-buffer
// step), after any reachability probe.
func execDelivered(execs []string) bool {
	for _, e := range execs {
		if strings.Contains(e, "load-buffer") {
			return true
		}
	}
	return false
}

// readyTestHarness is a delivery test harness with its own resolved ready
// spec.
type readyTestHarness struct {
	deliveryTestHarness
	ready api.ReadySpec
}

func (h *readyTestHarness) ReadySpec() api.ReadySpec { return h.ready }

// TestConduitDelivery_EffectiveReadySpecBounds (review P2-R1): the
// harness's resolved readiness spec, not the broker's default timings, is
// checked against the route bounds before conduit is chosen. Out of bounds
// uses exec (or fails, named, without exec); in bounds goes to the hub and
// carries the spec unchanged.
func TestConduitDelivery_EffectiveReadySpecBounds(t *testing.T) {
	cases := map[string]struct {
		spec  api.ReadySpec
		field string
	}{
		"settle 11s":  {api.ReadySpec{Strategy: "hook", Settle: 11 * time.Second, Timeout: 90 * time.Second}, "ready.settle"},
		"timeout 91s": {api.ReadySpec{Strategy: "hook", Settle: 1500 * time.Millisecond, Timeout: 91 * time.Second}, "ready.timeout"},
		"delay 11s":   {api.ReadySpec{Strategy: "delay", Settle: 11 * time.Second}, "ready.settle"},
	}
	for name, tc := range cases {
		for _, execOK := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s exec=%t", name, execOK), func(t *testing.T) {
				var execs []string
				m := newConduitTestManager(t, true, shippedSpec, &execs)
				m.newHarness = func(string) api.Harness {
					return &readyTestHarness{deliveryTestHarness: deliveryTestHarness{spec: shippedSpec}, ready: tc.spec}
				}
				if !execOK {
					m.Runtime.(*runtime.MockRuntime).ExecFunc = func(context.Context, string, []string) (string, error) {
						return "", errors.New("exec connector not configured")
					}
				}
				d := &fakeDeliverer{answers: []fakeAnswer{answer(200, agentcontrol.OutcomeDelivered)}}
				m.SetConduitDeliverer(d)
				err := m.deliver(context.Background(), deliveryRequest{agentID: "test-agent", message: "x", messageIDs: []string{"m"}})
				if d.calls() != 0 {
					t.Fatalf("hub called for an out-of-bounds readiness spec")
				}
				if execOK {
					if err != nil || !execDelivered(execs) {
						t.Fatalf("err %v execs %v; want exec", err, execs)
					}
					return
				}
				var final *finalDeliveryError
				if !errors.As(err, &final) || !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), "exec unavailable") {
					t.Fatalf("err = %v; want a final error naming %s", err, tc.field)
				}
			})
		}
	}
	t.Run("in bounds", func(t *testing.T) {
		var execs []string
		m := newConduitTestManager(t, true, shippedSpec, &execs)
		spec := api.ReadySpec{Strategy: "hook", Settle: 2 * time.Second, Timeout: 60 * time.Second}
		m.newHarness = func(string) api.Harness {
			return &readyTestHarness{deliveryTestHarness: deliveryTestHarness{spec: shippedSpec}, ready: spec}
		}
		d := &fakeDeliverer{answers: []fakeAnswer{answer(200, agentcontrol.OutcomeDelivered)}}
		m.SetConduitDeliverer(d)
		if err := m.deliver(context.Background(), deliveryRequest{agentID: "test-agent", message: "x", messageIDs: []string{"m"}, readyWanted: true}); err != nil {
			t.Fatal(err)
		}
		if d.calls() != 1 || len(execs) != 0 {
			t.Fatalf("calls %d execs %v", d.calls(), execs)
		}
		if r := d.reqs[0].Ready; r == nil || r.Strategy != "hook" || r.SettleMS != 2000 || r.TimeoutMS != 60000 {
			t.Fatalf("ready block = %+v, want the harness spec unchanged", r)
		}
	})
	t.Run("agent refuses its local spec", func(t *testing.T) {
		var execs []string
		m := newConduitTestManager(t, true, shippedSpec, &execs)
		d := &fakeDeliverer{answers: []fakeAnswer{{status: 422, resp: agentcontrol.DeliverResponse{Outcome: agentcontrol.OutcomeOutOfBounds, Field: "ready.settle"}}}}
		m.SetConduitDeliverer(d)
		if err := m.deliver(context.Background(), deliveryRequest{agentID: "test-agent", message: "x", messageIDs: []string{"m"}}); err != nil {
			t.Fatal(err)
		}
		if d.calls() != 1 || !execDelivered(execs) {
			t.Fatalf("calls %d execs %v; want one refusal then exec at once", d.calls(), execs)
		}
	})
}

// TestConduitDelivery_InFlightDuplicateNeverFails (review FV1-N1): when the
// hub answers that the same delivery is in flight under another call, this
// call ends without an error: no exec fallback, no retry, and no failure
// report that would fail the owner's rows (buffered callback or the
// interrupt's synchronous answer to ExecuteAgentDM).
func TestConduitDelivery_InFlightDuplicateNeverFails(t *testing.T) {
	inFlight := fakeAnswer{status: 409, resp: agentcontrol.DeliverResponse{Outcome: agentcontrol.OutcomeInFlight}}
	t.Run("interrupt", func(t *testing.T) {
		var execs []string
		m := newConduitTestManager(t, true, shippedSpec, &execs)
		d := &fakeDeliverer{answers: []fakeAnswer{inFlight}}
		m.SetConduitDeliverer(d)
		if err := m.Message(WithDeliveryMessageID(context.Background(), "m-1"), "test-agent", "", "stop", true); err != nil {
			t.Fatalf("interrupt answered %v; ExecuteAgentDM would fail the owner's row", err)
		}
		if d.calls() != 1 || len(execs) != 0 {
			t.Fatalf("calls %d execs %v; want one call and no exec", d.calls(), execs)
		}
	})
	t.Run("buffered", func(t *testing.T) {
		var execs []string
		m := newConduitTestManager(t, true, shippedSpec, &execs)
		d := &fakeDeliverer{answers: []fakeAnswer{inFlight}}
		m.SetConduitDeliverer(d)
		done := make(chan struct{})
		var failed atomic.Bool
		m.msgBuffer = NewMessageBuffer(10*time.Millisecond, func(string, string, string, bool) error { return nil })
		m.msgBuffer.deliverFlush = func(f bufferedFlush) error {
			defer close(done)
			return m.deliver(context.Background(), deliveryRequest{agentID: f.agentID, message: f.text, messageIDs: f.messageIDs})
		}
		defer m.msgBuffer.Close()
		m.msgBuffer.sendWithID("test-agent", "", "hello", "m-1", func(error) { failed.Store(true) }, nil)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("no flush")
		}
		time.Sleep(50 * time.Millisecond)
		if failed.Load() {
			t.Fatal("failure handler called: /message-failures would fail the owner's rows")
		}
		if d.calls() != 1 || len(execs) != 0 {
			t.Fatalf("calls %d execs %v", d.calls(), execs)
		}
	})
}

// warnPreDelivery is attachment work that failed, as on a runtime without
// exec: it appends the "not delivered" line to the text.
type warnPreDelivery struct{}

func (warnPreDelivery) BeforeText(context.Context, runtime.Runtime, string) string {
	return "\n[attachment report.pdf not delivered]"
}
func (warnPreDelivery) Discard() {}

// TestConduitDelivery_AttachmentsWithoutExec (review FV1-N2): where the
// runtime has no exec, a message with attachments is still delivered: the
// attachment work's "not delivered" lines are in the text, which goes over
// conduit. Where exec exists, attachments stay on exec
// (TestConduitDelivery_AttachmentsUseExec).
func TestConduitDelivery_AttachmentsWithoutExec(t *testing.T) {
	noExec := func(m *AgentManager) {
		rt := m.Runtime.(*runtime.MockRuntime)
		rt.ExecFunc = func(context.Context, string, []string) (string, error) {
			return "", errors.New("exec connector not configured")
		}
		rt.ExecWithStdinFunc = func(context.Context, string, []string, io.Reader) (string, error) {
			return "", errors.New("exec connector not configured")
		}
	}
	t.Run("interrupt", func(t *testing.T) {
		var execs []string
		m := newConduitTestManager(t, true, shippedSpec, &execs)
		noExec(m)
		d := &fakeDeliverer{answers: []fakeAnswer{answer(200, agentcontrol.OutcomeDelivered)}}
		m.SetConduitDeliverer(d)
		ctx := WithPreDelivery(WithDeliveryMessageID(context.Background(), "m-1"), warnPreDelivery{})
		if err := m.Message(ctx, "test-agent", "", "see file", true); err != nil {
			t.Fatal(err)
		}
		if d.calls() != 1 || !strings.Contains(*d.reqs[0].Text, "see file") || !strings.Contains(*d.reqs[0].Text, "not delivered") {
			t.Fatalf("calls %d; want the text with its warning over conduit", d.calls())
		}
	})
	t.Run("buffered", func(t *testing.T) {
		var execs []string
		m := newConduitTestManager(t, true, shippedSpec, &execs)
		noExec(m)
		d := &fakeDeliverer{answers: []fakeAnswer{answer(200, agentcontrol.OutcomeDelivered)}}
		m.SetConduitDeliverer(d)
		done := make(chan error, 1)
		m.msgBuffer = NewMessageBuffer(10*time.Millisecond, func(string, string, string, bool) error { return nil })
		m.msgBuffer.preDeliver = func(ctx context.Context, _, _ string, pd PreDelivery) string { return pd.BeforeText(ctx, nil, "") }
		m.msgBuffer.deliverFlush = func(f bufferedFlush) error {
			err := m.deliver(context.Background(), deliveryRequest{agentID: f.agentID, message: f.text,
				messageIDs: f.messageIDs, hasAttachments: f.hasAttachments})
			done <- err
			return err
		}
		defer m.msgBuffer.Close()
		m.msgBuffer.sendWithID("test-agent", "", "see file", "m-1", nil, warnPreDelivery{})
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no flush")
		}
		if d.calls() != 1 || !strings.Contains(*d.reqs[0].Text, "not delivered") {
			t.Fatalf("calls %d; want the text with its warning over conduit", d.calls())
		}
	})
}

// TestConduitDelivery_InterruptWaitsBehindFlush (review P2-O1, design §2.3):
// on conduit the injection lock is held across the hub call, which
// includes the agent's readiness wait, so an interrupt that arrives
// during a buffered flush waits for it and never overtakes it.
func TestConduitDelivery_InterruptWaitsBehindFlush(t *testing.T) {
	var execs []string
	m := newConduitTestManager(t, true, shippedSpec, &execs)
	entered, release := make(chan struct{}), make(chan struct{})
	var order []string
	var mu sync.Mutex
	d := &fakeDeliverer{answers: []fakeAnswer{answer(200, agentcontrol.OutcomeDelivered)}}
	first := true
	d.onCall = func() {
		mu.Lock()
		isFirst := first
		first = false
		mu.Unlock()
		if isFirst {
			close(entered)
			<-release
		}
	}
	m.SetConduitDeliverer(d)
	flushDone := make(chan struct{})
	go func() {
		defer close(flushDone)
		_ = m.deliver(context.Background(), deliveryRequest{agentID: "test-agent", message: "buffered", messageIDs: []string{"m-1"}, readyWanted: true})
		mu.Lock()
		order = append(order, "flush")
		mu.Unlock()
	}()
	<-entered
	intDone := make(chan struct{})
	go func() {
		defer close(intDone)
		_ = m.Message(WithDeliveryMessageID(context.Background(), "m-2"), "test-agent", "", "stop", true)
		mu.Lock()
		order = append(order, "interrupt")
		mu.Unlock()
	}()
	time.Sleep(100 * time.Millisecond)
	if d.calls() != 1 {
		t.Fatalf("the interrupt reached the hub (%d calls) while the flush held the lock", d.calls())
	}
	close(release)
	<-flushDone
	<-intDone
	if d.calls() != 2 || order[0] != "flush" || d.reqs[1].Op != agentcontrol.OpInterruptPaste {
		t.Fatalf("calls %d order %v", d.calls(), order)
	}
}
