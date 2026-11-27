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

// Package trajectory wires the Track 2 raw trajectory shipper into sciontool
// init and hook wake notifications.
package trajectory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/trajectory/shipper"
)

const (
	// EnvPeriodicInterval overrides the default periodic trajectory ship
	// interval (for example "2m" or "30s").
	EnvPeriodicInterval = "SCION_TRAJECTORY_PERIODIC_INTERVAL"

	// DefaultPeriodicInterval is the default interval between periodic
	// trajectory capture rounds in sciontool init.
	DefaultPeriodicInterval = 10 * time.Minute

	// DefaultModelEndDebounce is the debounce window for Claude Stop
	// (model-end) wake notifications before running a ship round.
	DefaultModelEndDebounce = 15 * time.Second

	// DefaultUnavailableBackoff is how long the runner backs off after a 404
	// (experiment off) or 403 policy refusal (after rebaselining).
	DefaultUnavailableBackoff = 10 * time.Minute

	// DefaultRoundTimeout bounds a single background capture round
	// (resume_catchup, periodic, session_end, model_end).
	DefaultRoundTimeout = 2 * time.Minute

	// DefaultPreStopTimeout bounds the Final=true pre_stop capture round so
	// trajectory shipping never delays container stop past telemetryStopBudget.
	DefaultPreStopTimeout = 5 * time.Second

	// DefaultPreStopMaxBytes bounds raw bytes attempted during pre_stop.
	DefaultPreStopMaxBytes int64 = 16 * 1024 * 1024

	// DefaultLockTimeout bounds lock acquisition for background rounds.
	DefaultLockTimeout = 30 * time.Second

	// WakeSocketName is the Unix domain datagram socket filename under
	// <home>/.scion/ used by hook subprocesses to wake the init runner.
	WakeSocketName = "trajectory-shipper-wake.sock"

	// WakeSessionEnd wakes the runner for an immediate session_end round.
	WakeSessionEnd byte = 's'

	// WakeModelEnd wakes the runner for a debounced model_end round.
	WakeModelEnd byte = 'm'
)

// WakeSocketPath returns the Unix domain socket path for hook wake datagrams.
func WakeSocketPath(agentHome string) string {
	if agentHome == "" {
		return ""
	}
	return filepath.Join(agentHome, ".scion", WakeSocketName)
}

// Config configures a Runner.
type Config struct {
	HomeDir            string
	RunID              string
	Harness            string
	OriginHubID        string
	Uploader           shipper.Uploader
	PeriodicInterval   time.Duration
	ModelEndDebounce   time.Duration
	UnavailableBackoff time.Duration
	RoundTimeout       time.Duration
	PreStopTimeout     time.Duration
	PreStopMaxBytes    int64
	LockTimeout        time.Duration
	OwnerUID           int
	OwnerGID           int
	Now                func() time.Time
}

// Runner coordinates trajectory shipping rounds inside sciontool init:
//   - resume_catchup once at startup (before the periodic ticker)
//   - periodic rounds on a configurable ticker (default 10m)
//   - session_end rounds triggered via the Unix wake socket from Claude hooks
//   - debounced (~15s) model_end rounds triggered via the wake socket on Stop
//   - bounded Final=true pre_stop flush on container shutdown
type Runner struct {
	cfg     Config
	shipper *shipper.Shipper
	now     func() time.Time

	mu           sync.Mutex
	started      bool
	stopped      bool
	backoffUntil time.Time
	loopCancel   context.CancelFunc
	roundCancel  context.CancelFunc
	wakeListener io.Closer
	doneCh       chan struct{}

	sessionEndCh chan struct{}
	modelEndCh   chan struct{}
	preStopOnce  sync.Once
}

// NewRunner creates a validated trajectory Runner.
func NewRunner(cfg Config) (*Runner, error) {
	if cfg.HomeDir == "" {
		return nil, errors.New("trajectory runner: HomeDir is required")
	}
	if cfg.RunID == "" {
		return nil, errors.New("trajectory runner: RunID is required")
	}
	if cfg.Uploader == nil {
		return nil, errors.New("trajectory runner: Uploader is required")
	}
	if cfg.Harness == "" {
		cfg.Harness = "claude"
	}
	if cfg.PeriodicInterval <= 0 {
		cfg.PeriodicInterval = DefaultPeriodicInterval
	}
	if cfg.ModelEndDebounce <= 0 {
		cfg.ModelEndDebounce = DefaultModelEndDebounce
	}
	if cfg.UnavailableBackoff <= 0 {
		cfg.UnavailableBackoff = DefaultUnavailableBackoff
	}
	if cfg.RoundTimeout <= 0 {
		cfg.RoundTimeout = DefaultRoundTimeout
	}
	if cfg.PreStopTimeout <= 0 {
		cfg.PreStopTimeout = DefaultPreStopTimeout
	}
	if cfg.PreStopMaxBytes <= 0 {
		cfg.PreStopMaxBytes = DefaultPreStopMaxBytes
	}
	if cfg.LockTimeout <= 0 {
		cfg.LockTimeout = DefaultLockTimeout
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	s, err := shipper.New(shipper.Config{
		HomeDir:     cfg.HomeDir,
		RunID:       cfg.RunID,
		Harness:     cfg.Harness,
		OriginHubID: cfg.OriginHubID,
		LockTimeout: cfg.LockTimeout,
		Uploader:    cfg.Uploader,
		Now:         nowFn,
	})
	if err != nil {
		return nil, err
	}

	return &Runner{
		cfg:          cfg,
		shipper:      s,
		now:          nowFn,
		sessionEndCh: make(chan struct{}, 1),
		modelEndCh:   make(chan struct{}, 1),
	}, nil
}

// Start launches the wake listener (best-effort) and the background shipping
// loop (starting with a single resume_catchup round).
func (r *Runner) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return nil
	}
	r.started = true
	loopCtx, loopCancel := context.WithCancel(ctx)
	r.loopCancel = loopCancel
	r.doneCh = make(chan struct{})
	r.mu.Unlock()

	if sockPath := WakeSocketPath(r.cfg.HomeDir); sockPath != "" {
		l, err := startWakeListener(sockPath, r.cfg.OwnerUID, r.cfg.OwnerGID, r.handleWakeByte)
		if err != nil {
			log.Debug("Trajectory wake listener unavailable (periodic and pre_stop still active): %v", err)
		} else {
			r.mu.Lock()
			r.wakeListener = l
			r.mu.Unlock()
		}
	}

	go r.loop(loopCtx)
	return nil
}

func (r *Runner) handleWakeByte(b byte) {
	switch b {
	case WakeSessionEnd:
		select {
		case r.sessionEndCh <- struct{}{}:
		default:
		}
	case WakeModelEnd:
		select {
		case r.modelEndCh <- struct{}{}:
		default:
		}
	}
}

func (r *Runner) loop(ctx context.Context) {
	defer close(r.doneCh)

	// 1. Run resume_catchup once at startup before the periodic ticker begins.
	r.executeRound(ctx, shipper.RoundOptions{
		Final:  false,
		Reason: shipper.ReasonResumeCatchup,
	}, r.cfg.RoundTimeout)

	ticker := time.NewTicker(r.cfg.PeriodicInterval)
	defer ticker.Stop()

	var debounceTimer *time.Timer
	var debounceC <-chan time.Time
	stopDebounce := func() {
		if debounceTimer != nil {
			if !debounceTimer.Stop() {
				select {
				case <-debounceTimer.C:
				default:
				}
			}
			debounceTimer = nil
			debounceC = nil
		}
	}
	defer stopDebounce()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			stopDebounce()
			r.executeRound(ctx, shipper.RoundOptions{
				Final:  false,
				Reason: shipper.ReasonPeriodic,
			}, r.cfg.RoundTimeout)

		case <-r.sessionEndCh:
			stopDebounce()
			r.executeRound(ctx, shipper.RoundOptions{
				Final:  false,
				Reason: shipper.ReasonSessionEnd,
			}, r.cfg.RoundTimeout)

		case <-r.modelEndCh:
			if debounceTimer == nil {
				debounceTimer = time.NewTimer(r.cfg.ModelEndDebounce)
				debounceC = debounceTimer.C
			} else {
				if !debounceTimer.Stop() {
					select {
					case <-debounceTimer.C:
					default:
					}
				}
				debounceTimer.Reset(r.cfg.ModelEndDebounce)
			}

		case <-debounceC:
			debounceTimer = nil
			debounceC = nil
			r.executeRound(ctx, shipper.RoundOptions{
				Final:  false,
				Reason: shipper.ReasonModelEnd,
			}, r.cfg.RoundTimeout)
		}
	}
}

// PreStop stops the background loop (cancelling any in-flight non-final round
// so state.lock is released promptly), closes the wake listener, and runs a
// single bounded Final=true pre_stop round. Safe to call multiple times; only
// the first call executes.
func (r *Runner) PreStop(ctx context.Context) {
	if r == nil {
		return
	}
	r.preStopOnce.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		preStopCtx, cancel := context.WithTimeout(ctx, r.cfg.PreStopTimeout)
		defer cancel()

		r.mu.Lock()
		r.stopped = true
		loopCancel := r.loopCancel
		roundCancel := r.roundCancel
		doneCh := r.doneCh
		wake := r.wakeListener
		r.wakeListener = nil
		r.mu.Unlock()

		if roundCancel != nil {
			roundCancel()
		}
		if loopCancel != nil {
			loopCancel()
		}
		if wake != nil {
			_ = wake.Close()
		}
		if doneCh != nil {
			select {
			case <-doneCh:
			case <-preStopCtx.Done():
				log.Warn("Trajectory pre_stop: timed out waiting for background loop to exit")
				return
			}
		}

		// Bound lock timeout for pre_stop to the remaining stop budget.
		r.executeRound(preStopCtx, shipper.RoundOptions{
			Final:    true,
			Reason:   shipper.ReasonPreStop,
			MaxBytes: r.cfg.PreStopMaxBytes,
		}, r.cfg.PreStopTimeout)
	})
}

func (r *Runner) inBackoff() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.backoffUntil.IsZero() && r.now().Before(r.backoffUntil)
}

func (r *Runner) setBackoff() {
	r.mu.Lock()
	r.backoffUntil = r.now().Add(r.cfg.UnavailableBackoff)
	r.mu.Unlock()
}

func (r *Runner) executeRound(parent context.Context, opts shipper.RoundOptions, timeout time.Duration) {
	if r.inBackoff() {
		return
	}
	if err := parent.Err(); err != nil {
		return
	}

	roundCtx, cancel := context.WithTimeout(parent, timeout)
	r.mu.Lock()
	r.roundCancel = cancel
	r.mu.Unlock()
	defer func() {
		cancel()
		r.mu.Lock()
		r.roundCancel = nil
		r.mu.Unlock()
	}()

	res, err := r.shipper.RunRound(roundCtx, opts)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		// Never log payload contents; log only operational error details.
		log.Warn("Trajectory ship round (%s) failed: %v", opts.Reason, err)
		return
	}
	if res == nil {
		return
	}

	if res.PolicyRefused {
		reason := res.PolicyRefusedReason
		rebaselineCtx, rebaselineCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if _, rerr := r.shipper.Rebaseline(rebaselineCtx, reason); rerr != nil {
			log.Warn("Trajectory policy refusal (%s): rebaseline failed: %v", reason, rerr)
		} else {
			log.Info("Trajectory capture refused by policy (%s); rebaselined to EOF and backing off", reason)
		}
		rebaselineCancel()
		r.setBackoff()
		return
	}

	if res.CaptureUnavailable {
		log.Debug("Trajectory capture unavailable on Hub (404); backing off for %s", r.cfg.UnavailableBackoff)
		r.setBackoff()
		return
	}

	if res.Truncated {
		log.Warn("Trajectory capture run cap exceeded for run %s; stopping shipping for this run", r.cfg.RunID)
		return
	}

	if len(res.ArtifactErrors) > 0 {
		log.Warn("Trajectory ship round (%s) encountered %d artifact error(s)", opts.Reason, len(res.ArtifactErrors))
	}
	if res.SegmentsShipped > 0 || res.GenerationBumps > 0 {
		log.Info("Trajectory ship round (%s): segments=%d bytes=%d bumps=%d",
			opts.Reason, res.SegmentsShipped, res.BytesShipped, res.GenerationBumps)
	}
}

// ResolveRunID resolves the agent's run ID for shipper.Config.RunID, checking:
//  1. client.RunID() (populated from SCION_LAUNCH_ID),
//  2. os.Getenv(hub.EnvLaunchID),
//  3. the "run_id" claim in the agent JWT payload (unverified base64url decode).
func ResolveRunID(client *hub.Client) string {
	if client != nil {
		if rid := strings.TrimSpace(client.RunID()); rid != "" {
			return rid
		}
	}
	if rid := strings.TrimSpace(os.Getenv(hub.EnvLaunchID)); rid != "" {
		return rid
	}
	if client != nil {
		if rid := extractRunIDFromJWT(client.AuthToken()); rid != "" {
			return rid
		}
	}
	return ""
}

func extractRunIDFromJWT(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return strings.TrimSpace(claims.RunID)
}
