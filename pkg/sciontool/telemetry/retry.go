/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"math"
	"math/rand/v2"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// Default retry configuration constants.
const (
	defaultMaxRetries     = 3
	defaultInitialBackoff = 100 * time.Millisecond
	defaultMaxBackoff     = 5 * time.Second
	defaultBackoffMult    = 2.0
	defaultJitterMin      = 0.10
	defaultJitterMax      = 0.20
)

// RetryConfig holds configuration for export retry behavior.
type RetryConfig struct {
	// MaxRetries is the maximum number of retry attempts after the initial call.
	MaxRetries int
	// InitialBackoff is the delay before the first retry.
	InitialBackoff time.Duration
	// MaxBackoff caps the backoff duration.
	MaxBackoff time.Duration
	// BackoffMultiplier scales the backoff after each attempt.
	BackoffMultiplier float64
	// JitterMin is the minimum jitter fraction (e.g. 0.10 for 10%).
	JitterMin float64
	// JitterMax is the maximum jitter fraction (e.g. 0.20 for 20%).
	JitterMax float64
}

// DefaultRetryConfig returns the default retry configuration.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:        defaultMaxRetries,
		InitialBackoff:    defaultInitialBackoff,
		MaxBackoff:        defaultMaxBackoff,
		BackoffMultiplier: defaultBackoffMult,
		JitterMin:         defaultJitterMin,
		JitterMax:         defaultJitterMax,
	}
}

// isRetryable reports whether the error should be retried based on its
// classification. Auth errors are not retryable because they will not
// self-heal without credential rotation.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	// Known retryable classes: timeout, quota, other (unknown).
	// Only auth is explicitly non-retryable.
	return classifyError(err) != "auth"
}

// retryExport retries fn with exponential backoff until it succeeds, a
// non-retryable error is encountered, retries are exhausted, or the context
// is cancelled. It returns the last error when all retries are exhausted.
func retryExport(ctx context.Context, cfg RetryConfig, signal string, fn func() error) error {
	err := fn()
	if err == nil {
		return nil
	}

	if !isRetryable(err) {
		log.Debug("Export %s failed with non-retryable error (%s): %v", signal, classifyError(err), err)
		return err
	}

	backoff := cfg.InitialBackoff
	for attempt := 1; attempt <= cfg.MaxRetries; attempt++ {
		// Apply jitter: multiply backoff by (1 + random(jitterMin, jitterMax))
		jitter := cfg.JitterMin + rand.Float64()*(cfg.JitterMax-cfg.JitterMin)
		sleep := time.Duration(float64(backoff) * (1 + jitter))

		log.Debug("Export %s retry %d/%d after %v (error: %v)", signal, attempt, cfg.MaxRetries, sleep, err)

		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}

		err = fn()
		if err == nil {
			log.Debug("Export %s succeeded on retry %d", signal, attempt)
			return nil
		}

		if !isRetryable(err) {
			log.Debug("Export %s retry %d hit non-retryable error (%s): %v", signal, attempt, classifyError(err), err)
			return err
		}

		// Increase backoff with multiplier, capped at MaxBackoff.
		backoff = time.Duration(math.Min(
			float64(backoff)*cfg.BackoffMultiplier,
			float64(cfg.MaxBackoff),
		))
	}

	return err
}
