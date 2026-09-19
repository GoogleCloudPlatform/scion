package commands

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInitTelemetryStopBudgetAndExpiry(t *testing.T) {
	if telemetryStopBudget != 20*time.Second {
		t.Fatalf("telemetry Stop budget = %s", telemetryStopBudget)
	}
	var observed time.Duration
	err := stopTelemetryWithTimeout(func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("telemetry Stop has no deadline")
		}
		observed = time.Until(deadline)
		return nil
	}, telemetryStopBudget)
	if err != nil || observed < 19*time.Second || observed > telemetryStopBudget {
		t.Fatalf("Stop call budget = %s, error = %v", observed, err)
	}
	err = stopTelemetryWithTimeout(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}, 10*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired Stop context = %v", err)
	}
}
