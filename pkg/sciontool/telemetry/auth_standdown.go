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

type authStandDown struct {
	mu       sync.Mutex
	active   bool
	until    time.Time
	probing  bool
	interval time.Duration
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

// allowCloudExport reports whether a cloud export call may proceed. When
// standing down after a permanent authentication failure, it returns
// (false, false) until the stand-down window expires, at which point it admits
// a single periodic re-check probe and returns (true, true).
func (p *Pipeline) allowCloudExport() (allow bool, isProbe bool) {
	p.authStandDown.mu.Lock()
	defer p.authStandDown.mu.Unlock()
	if !p.authStandDown.active {
		return true, false
	}
	if p.now().Before(p.authStandDown.until) || p.authStandDown.probing {
		return false, false
	}
	p.authStandDown.probing = true
	return true, true
}

// isAuthStandingDown reports whether cloud export is currently within an
// active auth stand-down window.
func (p *Pipeline) isAuthStandingDown() bool {
	p.authStandDown.mu.Lock()
	defer p.authStandDown.mu.Unlock()
	return p.authStandDown.active && p.now().Before(p.authStandDown.until)
}

// recordCloudExportOutcome updates auth stand-down state after a cloud export
// attempt. It returns true when it has already handled logging for a permanent
// auth failure (one clear error on entry, debug on subsequent failed probes),
// so the caller should suppress its per-batch error log.
func (p *Pipeline) recordCloudExportOutcome(signal string, err error, isProbe bool) (suppressedLog bool) {
	if err == nil {
		if isProbe {
			p.authStandDown.mu.Lock()
			p.authStandDown.active = false
			p.authStandDown.probing = false
			p.authStandDown.until = time.Time{}
			p.authStandDown.mu.Unlock()
			log.Info("Telemetry cloud export recovered after authentication re-check (%s)", signal)
		}
		return false
	}

	if isPermanentAuthError(err) {
		interval := p.authStandDownDuration()
		p.authStandDown.mu.Lock()
		wasActive := p.authStandDown.active
		p.authStandDown.active = true
		p.authStandDown.probing = false
		p.authStandDown.until = p.now().Add(interval)
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
		p.authStandDown.probing = false
		p.authStandDown.until = p.now().Add(interval)
		p.authStandDown.mu.Unlock()
	}
	return false
}
