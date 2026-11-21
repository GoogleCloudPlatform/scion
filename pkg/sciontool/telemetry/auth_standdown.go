/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// defaultAuthStandDownInterval is how long cloud export stands down after a
// permanent authentication failure before allowing a single periodic re-check.
const defaultAuthStandDownInterval = 5 * time.Minute

type signalAuthStandDown struct {
	active  bool
	until   time.Time
	probing bool
}

type authStandDown struct {
	mu       sync.Mutex
	interval time.Duration
	signals  map[string]*signalAuthStandDown
}

func (a *authStandDown) stateLocked(signal string) *signalAuthStandDown {
	if a.signals == nil {
		a.signals = make(map[string]*signalAuthStandDown)
	}
	st := a.signals[signal]
	if st == nil {
		st = &signalAuthStandDown{}
		a.signals[signal] = st
	}
	return st
}

// isPermanentAuthError reports whether err is a permanent authentication or
// authorization failure (e.g. HTTP 401/403, gRPC Unauthenticated or
// PermissionDenied).
func isPermanentAuthError(err error) bool {
	return err != nil && classifyError(err) == "auth"
}

func (p *Pipeline) authStandDownDuration() time.Duration {
	if p.authStandDown.interval > 0 {
		return p.authStandDown.interval
	}
	return defaultAuthStandDownInterval
}

// allowCloudExport reports whether a cloud export call for signal may proceed.
// Stand-down state is tracked per signal so a permission gap on one signal
// (e.g. traces) does not suppress another signal (e.g. metrics). When standing
// down after a permanent authentication failure, it returns (false, false)
// until the stand-down window expires, at which point it admits a single
// periodic re-check probe and returns (true, true).
func (p *Pipeline) allowCloudExport(signal string) (allow bool, isProbe bool) {
	p.authStandDown.mu.Lock()
	defer p.authStandDown.mu.Unlock()
	st := p.authStandDown.stateLocked(signal)
	if !st.active {
		return true, false
	}
	if p.now().Before(st.until) || st.probing {
		return false, false
	}
	st.probing = true
	return true, true
}

// releaseCloudExportProbe clears the in-flight probe flag for signal when a
// caller granted a probe exits before attempting the cloud export call.
func (p *Pipeline) releaseCloudExportProbe(signal string) {
	p.authStandDown.mu.Lock()
	defer p.authStandDown.mu.Unlock()
	if st := p.authStandDown.signals[signal]; st != nil {
		st.probing = false
	}
}

// isAuthStandingDown reports whether cloud export for signal is currently
// within an active auth stand-down window.
func (p *Pipeline) isAuthStandingDown(signal string) bool {
	p.authStandDown.mu.Lock()
	defer p.authStandDown.mu.Unlock()
	st := p.authStandDown.signals[signal]
	return st != nil && st.active && p.now().Before(st.until)
}

// recordCloudExportOutcome updates auth stand-down state for signal after a
// cloud export attempt. It returns true when it has already handled logging
// for a permanent auth failure (one clear error on entry, debug on subsequent
// failed probes), so the caller should suppress its per-batch error log.
func (p *Pipeline) recordCloudExportOutcome(signal string, err error, isProbe bool) (suppressedLog bool) {
	if err == nil {
		if isProbe {
			p.authStandDown.mu.Lock()
			st := p.authStandDown.stateLocked(signal)
			st.active = false
			st.probing = false
			st.until = time.Time{}
			p.authStandDown.mu.Unlock()
			log.Info("Telemetry cloud export recovered after authentication re-check (%s)", signal)
		}
		return false
	}

	if isPermanentAuthError(err) {
		interval := p.authStandDownDuration()
		p.authStandDown.mu.Lock()
		st := p.authStandDown.stateLocked(signal)
		wasActive := st.active
		st.active = true
		st.probing = false
		st.until = p.now().Add(interval)
		p.authStandDown.mu.Unlock()

		if !wasActive {
			log.Error("Telemetry cloud export standing down after permanent authentication failure (%s): %v; suppressing further export attempts until re-check in %v", signal, err, interval)
		} else {
			log.Debug("Telemetry cloud export auth re-check still failing (%s): %v; next re-check in %v", signal, err, interval)
		}
		return true
	}

	if isProbe {
		interval := p.authStandDownDuration()
		p.authStandDown.mu.Lock()
		st := p.authStandDown.stateLocked(signal)
		st.probing = false
		st.until = p.now().Add(interval)
		p.authStandDown.mu.Unlock()
	}
	return false
}
