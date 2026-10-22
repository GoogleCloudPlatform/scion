/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// Events are held (never sent without hub_id) until the hub ID resolves;
// send failures back off 1s, 2s, ... capped at 5 min; success resets.
func TestAnalyticsDrainerHoldsUntilHubAndBacksOff(t *testing.T) {
	o := testOutbox(t)
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:a", 1), AppendWritten)
	resolveErr := errors.New("hub unreachable")
	c := &capture{}
	d := NewAnalyticsDrainer(o, c.send, func(context.Context) (string, error) {
		if resolveErr != nil {
			return "", resolveErr
		}
		return "hub-1", nil
	})
	clock := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return clock }

	if n := d.RunOnce(context.Background()); n != 0 || len(c.lines) != 0 {
		t.Fatalf("sent %d before hub resolved", n)
	}
	if o.Stats().PendingBytes == 0 {
		t.Fatal("held event must stay pending")
	}

	resolveErr = nil
	c.fail = errors.New("logging unavailable")
	d.RunOnce(context.Background())
	if d.Backoff() != time.Second {
		t.Errorf("first backoff = %v, want 1s", d.Backoff())
	}
	d.RunOnce(context.Background()) // still within backoff: no attempt
	if got := o.Stats().SendFailures; got != 1 {
		t.Errorf("send attempts during backoff: failures = %d, want 1", got)
	}
	clock = clock.Add(2 * time.Second)
	d.RunOnce(context.Background())
	if d.Backoff() != 2*time.Second {
		t.Errorf("second backoff = %v, want 2s", d.Backoff())
	}
	for i := 0; i < 20; i++ {
		clock = clock.Add(10 * time.Minute)
		d.RunOnce(context.Background())
	}
	if d.Backoff() != analyticsBackoffMax {
		t.Errorf("backoff = %v, want cap %v", d.Backoff(), analyticsBackoffMax)
	}

	c.fail = nil
	clock = clock.Add(10 * time.Minute)
	if n := d.RunOnce(context.Background()); n != 1 || d.Backoff() != 0 {
		t.Errorf("recovery sent %d, backoff %v", n, d.Backoff())
	}
	if ev := decodeUsage(t, c.lines[0]); ev.HubID != "hub-1" {
		t.Errorf("sent hub_id = %q", ev.HubID)
	}
}

// Stop makes a final attempt even while backing off (graceful shutdown).
func TestAnalyticsDrainerStopFinalAttempt(t *testing.T) {
	o := testOutbox(t)
	_, _ = o.PinHubID("hub")
	c := &capture{fail: errors.New("down")}
	d := NewAnalyticsDrainer(o, c.send, nil)
	d.interval = time.Hour
	d.Start(context.Background())
	mustAppend(t, o, draftUsage(telemetrycontract.SourceIdentityComposite, "cmp1:a", 1), AppendWritten)
	c.mu.Lock()
	c.fail = nil
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.Stop(ctx)
	if len(c.lines) != 1 {
		t.Errorf("final drain sent %d, want 1", len(c.lines))
	}
}
