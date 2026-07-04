package discord

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLocker is a test double for AdvisoryLocker that tracks acquire/release
// calls and lets tests control lock behavior per call.
type fakeLocker struct {
	mu            sync.Mutex
	acquireResult bool
	acquireErr    error
	verifyErr     error

	acquireCount int32
	releases     int32
	lastKey      int64
}

func (f *fakeLocker) setAcquirable(b bool) {
	f.mu.Lock()
	f.acquireResult = b
	f.mu.Unlock()
}

func (f *fakeLocker) setVerifyErr(err error) {
	f.mu.Lock()
	f.verifyErr = err
	f.mu.Unlock()
}

func (f *fakeLocker) TryAdvisoryLock(_ context.Context, key int64) (bool, *AdvisoryLockHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	atomic.AddInt32(&f.acquireCount, 1)
	f.lastKey = key

	if f.acquireErr != nil {
		return false, nil, f.acquireErr
	}
	if !f.acquireResult {
		return false, nil, nil
	}

	return true, NewAdvisoryLockHandle(
		func() error {
			atomic.AddInt32(&f.releases, 1)
			return nil
		},
		func(ctx context.Context) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.verifyErr
		},
	), nil
}

func newTestLockLoop(locker *fakeLocker) *GatewayLockLoop {
	return NewGatewayLockLoop(locker, 0x5C100009, slog.Default())
}

func TestGatewayLockLoop_TakeoverDelay(t *testing.T) {
	locker := &fakeLocker{acquireResult: true}
	loop := newTestLockLoop(locker)

	acquiredCount := 0
	loop.OnAcquired = func() error {
		acquiredCount++
		return nil
	}

	ctx := context.Background()

	// Tick 1: acquires lock but releases (takeover delay, need 2 consecutive).
	loop.Tick(ctx)
	assert.False(t, loop.Active(), "should not be active after first tick")
	assert.Equal(t, int32(1), atomic.LoadInt32(&locker.releases), "should release after first tick")
	assert.Equal(t, 0, acquiredCount, "OnAcquired should not fire yet")

	// Tick 2: acquires again, takeover delay satisfied → OnAcquired fires.
	loop.Tick(ctx)
	assert.True(t, loop.Active(), "should be active after second tick")
	assert.Equal(t, 1, acquiredCount, "OnAcquired should fire exactly once")
}

func TestGatewayLockLoop_TakeoverDelayResetOnFailure(t *testing.T) {
	locker := &fakeLocker{acquireResult: true}
	loop := newTestLockLoop(locker)
	loop.OnAcquired = func() error { return nil }

	ctx := context.Background()

	// Tick 1: lock acquirable → counter=1, released.
	loop.Tick(ctx)
	assert.False(t, loop.Active())

	// Lock becomes unavailable.
	locker.setAcquirable(false)

	// Tick 2: not acquirable → counter resets.
	loop.Tick(ctx)
	assert.False(t, loop.Active())

	// Lock available again.
	locker.setAcquirable(true)

	// Tick 3: acquirable → counter=1 (reset), released.
	loop.Tick(ctx)
	assert.False(t, loop.Active(), "counter should have reset, need 2 consecutive")

	// Tick 4: acquirable → counter=2, promoted.
	loop.Tick(ctx)
	assert.True(t, loop.Active())
}

func TestGatewayLockLoop_SubscribeFailureReleasesLock(t *testing.T) {
	locker := &fakeLocker{acquireResult: true}
	loop := newTestLockLoop(locker)

	subscribeErr := errors.New("gateway connect failed")
	loop.OnAcquired = func() error { return subscribeErr }

	ctx := context.Background()

	// Two ticks to satisfy takeover delay.
	loop.Tick(ctx)
	loop.Tick(ctx)

	assert.False(t, loop.Active(), "should not be active when OnAcquired fails")
	// 1 release from takeover delay tick + 1 from failed acquire
	assert.Equal(t, int32(2), atomic.LoadInt32(&locker.releases))
}

func TestGatewayLockLoop_LockLossClosesGateway(t *testing.T) {
	locker := &fakeLocker{acquireResult: true}
	loop := newTestLockLoop(locker)

	loop.OnAcquired = func() error { return nil }

	lostCalled := false
	loop.OnLost = func() { lostCalled = true }

	ctx := context.Background()

	// Acquire lock (2 ticks for takeover delay).
	loop.Tick(ctx)
	loop.Tick(ctx)
	require.True(t, loop.Active())

	// Simulate lock connection death.
	locker.setVerifyErr(errors.New("connection dead"))

	// Next tick verifies → detects loss.
	loop.Tick(ctx)

	assert.False(t, loop.Active(), "should be inactive after lock loss")
	assert.True(t, lostCalled, "OnLost should fire")
}

func TestGatewayLockLoop_LockLossContextCancelled(t *testing.T) {
	locker := &fakeLocker{acquireResult: true}
	loop := newTestLockLoop(locker)

	loop.OnAcquired = func() error { return nil }

	lostCalled := false
	loop.OnLost = func() { lostCalled = true }

	ctx := context.Background()

	// Acquire.
	loop.Tick(ctx)
	loop.Tick(ctx)
	require.True(t, loop.Active())

	locker.setVerifyErr(errors.New("connection dead"))

	// Verify with a cancelled context — should NOT treat as lock loss.
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	loop.Tick(cancelledCtx)

	assert.True(t, loop.Active(), "should stay active when ctx is cancelled")
	assert.False(t, lostCalled, "OnLost should not fire during shutdown")
}

func TestGatewayLockLoop_StandbyNotAcquirable(t *testing.T) {
	locker := &fakeLocker{acquireResult: false}
	loop := newTestLockLoop(locker)

	acquiredCount := 0
	loop.OnAcquired = func() error {
		acquiredCount++
		return nil
	}

	ctx := context.Background()

	for i := 0; i < 5; i++ {
		loop.Tick(ctx)
	}

	assert.False(t, loop.Active())
	assert.Equal(t, 0, acquiredCount, "OnAcquired should never fire")
}

func TestGatewayLockLoop_AcquireError(t *testing.T) {
	locker := &fakeLocker{acquireErr: errors.New("db down")}
	loop := newTestLockLoop(locker)

	loop.OnAcquired = func() error { return nil }

	ctx := context.Background()
	loop.Tick(ctx)

	assert.False(t, loop.Active())
}

func TestGatewayLockLoop_VerifyWhileActiveKeepsLock(t *testing.T) {
	locker := &fakeLocker{acquireResult: true}
	loop := newTestLockLoop(locker)

	loop.OnAcquired = func() error { return nil }

	ctx := context.Background()

	// Acquire.
	loop.Tick(ctx)
	loop.Tick(ctx)
	require.True(t, loop.Active())

	// Multiple verify ticks with healthy conn — should stay active.
	for i := 0; i < 5; i++ {
		loop.Tick(ctx)
		assert.True(t, loop.Active())
	}
}

func TestGatewayLockLoop_ReleaseHandleWithoutOnLost(t *testing.T) {
	locker := &fakeLocker{acquireResult: true}
	loop := newTestLockLoop(locker)

	loop.OnAcquired = func() error { return nil }
	lostCalled := false
	loop.OnLost = func() { lostCalled = true }

	ctx := context.Background()

	loop.Tick(ctx)
	loop.Tick(ctx)
	require.True(t, loop.Active())

	// Orderly shutdown: ReleaseHandle should NOT call OnLost.
	loop.ReleaseHandle()

	assert.False(t, loop.Active())
	assert.False(t, lostCalled, "OnLost must not fire during orderly shutdown")
}

func TestGatewayLockLoop_RunExitsOnCancel(t *testing.T) {
	locker := &fakeLocker{acquireResult: false}
	loop := newTestLockLoop(locker)
	loop.LockInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		loop.Run(ctx)
		close(done)
	}()

	// Let a few ticks fire.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after context cancellation")
	}
}

func TestGatewayLockLoop_ConcurrentAccess(t *testing.T) {
	locker := &fakeLocker{acquireResult: true}
	loop := newTestLockLoop(locker)
	loop.LockInterval = 5 * time.Millisecond

	loop.OnAcquired = func() error { return nil }
	loop.OnLost = func() {}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		loop.Run(ctx)
		close(done)
	}()

	// Concurrently read Active and call ReleaseHandle.
	time.Sleep(30 * time.Millisecond)
	_ = loop.Active()
	cancel()
	<-done
	loop.ReleaseHandle()

	assert.False(t, loop.Active())
}
