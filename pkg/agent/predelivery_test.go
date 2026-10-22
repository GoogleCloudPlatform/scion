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
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// recordingPreDelivery records when it ran and returns suffix.
type recordingPreDelivery struct {
	name      string
	suffix    string
	log       *[]string
	mu        *sync.Mutex
	discarded bool
}

func (p *recordingPreDelivery) BeforeText(context.Context, runtime.Runtime, string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	*p.log = append(*p.log, "pre:"+p.name)
	return p.suffix
}

func (p *recordingPreDelivery) Discard() { p.discarded = true }

// TestInboxWriter_WritesBeforeText: each buffered message's pre-delivery
// work (the attachment write) runs once, in message order, before the
// coalesced text is delivered and outside its retries, and its text
// follows that message's own text (ptone/scion#4596).
func TestInboxWriter_WritesBeforeText(t *testing.T) {
	var mu sync.Mutex
	var log []string
	delivered := make(chan string, 4)
	attempts := 0
	buf := NewMessageBuffer(50*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		mu.Lock()
		log = append(log, "deliver")
		attempts++
		first := attempts == 1
		mu.Unlock()
		if first {
			return errors.New("transient")
		}
		delivered <- message
		return nil
	})
	buf.flushRetryBackoff = time.Millisecond
	buf.preDeliver = func(ctx context.Context, agentID, projectID string, pd PreDelivery) string {
		return pd.BeforeText(ctx, nil, "")
	}
	t.Cleanup(buf.Close)
	one := &recordingPreDelivery{name: "one", suffix: "\n\nline one", log: &log, mu: &mu}
	two := &recordingPreDelivery{name: "two", suffix: "", log: &log, mu: &mu}
	buf.SendWithHooks("agent-1", "p", "first", nil, one)
	buf.SendWithHooks("agent-1", "p", "second", nil, two)
	select {
	case got := <-delivered:
		if got != "first\n\nline one\n\nsecond" {
			t.Fatalf("delivered %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("not delivered")
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"pre:one", "pre:two", "deliver", "deliver"}
	if len(log) != len(want) {
		t.Fatalf("order = %v, want %v", log, want)
	}
	for i := range want {
		if log[i] != want[i] {
			t.Fatalf("order = %v, want %v", log, want)
		}
	}
}

// TestPreDelivery_DiscardedWhenBufferClosed: a message sent after Close
// discards its pre-delivery work.
func TestPreDelivery_DiscardedWhenBufferClosed(t *testing.T) {
	buf := NewMessageBuffer(time.Hour, func(string, string, string, bool) error { return nil })
	buf.Close()
	var mu sync.Mutex
	var log []string
	pd := &recordingPreDelivery{name: "x", log: &log, mu: &mu}
	buf.SendWithHooks("agent-1", "p", "late", nil, pd)
	if !pd.discarded {
		t.Fatal("the pre-delivery work of a dropped message is discarded")
	}
}
