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

package autoexpose

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/backoff"
	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// autoExposeLabel is the label applied to auto-exposed ports so operators
// can distinguish them from manually-exposed ports.
const autoExposeLabel = "auto-scan"

// HubPortClient is the subset of hub.Client needed by the reconciler.
// Using an interface enables unit testing without a real Hub.
type HubPortClient interface {
	RegisterPort(ctx context.Context, req scionhub.RegisterPortRequest) (*scionhub.ExposedPort, error)
	DeletePort(ctx context.Context, port int) error
	ListPorts(ctx context.Context) ([]scionhub.ExposedPort, error)
}

// MessageClient sends messages via the Hub API.
// Used to notify the agent when ports are auto-exposed.
type MessageClient interface {
	SendSelfMessage(ctx context.Context, msg string, metadata map[string]string) error
}

// scanFunc is the function signature for scanning listeners.
// Defaults to ScanListeners; overridable in tests.
type scanFunc func() ([]ListenSocket, error)

// Reconciler periodically scans for listening TCP ports and reconciles
// them with the Hub's port registry. It only manages ports it auto-exposed
// (tracked in a local set) and never un-exposes manually-registered ports.
type Reconciler struct {
	client    HubPortClient
	msgClient MessageClient // optional; nil means no notifications
	cfg       Config
	scan      scanFunc

	// autoExposed tracks ports this reconciler has registered.
	// Only these ports are candidates for auto-unexpose.
	autoExposed map[int]bool

	// cachedRegistered is the last-known set of registered ports (port -> exposedBy).
	// Refreshed after register/delete operations or every cacheRefreshInterval ticks.
	cachedRegistered  map[int]string
	ticksSinceRefresh int

	// backoff paces reconciles after failures; state is the loop's
	// current health, used to log only on changes.
	backoff backoff.Backoff
	state   loopState

	// after returns a channel that fires after d. Defaults to time.After;
	// overridable in tests.
	after func(d time.Duration) <-chan time.Time
}

// loopState is the health of the reconcile loop.
type loopState int

const (
	stateHealthy      loopState = iota
	stateBackoff                // recent reconciles failed; delays grow
	stateUnauthorized           // the hub answered 401; retry at the cap
)

// DefaultMaxBackoff caps the delay between reconciles after failures,
// including while the hub does not accept the agent's credential.
const DefaultMaxBackoff = 5 * time.Minute

// cacheRefreshInterval is the number of ticks between forced cache refreshes.
// Between refreshes, the reconciler uses its local cache to avoid HTTP calls.
const cacheRefreshInterval = 10

// NewReconciler creates a new auto-expose reconciler.
func NewReconciler(client HubPortClient, cfg Config) *Reconciler {
	return &Reconciler{
		client:      client,
		cfg:         cfg,
		scan:        ScanListeners,
		autoExposed: make(map[int]bool),
		backoff: backoff.Backoff{
			Base:   cfg.Interval,
			Max:    max(DefaultMaxBackoff, cfg.Interval),
			Jitter: backoff.DefaultJitter,
		},
		after: time.After,
	}
}

// SetMessageClient sets the optional message client used to notify the agent
// when ports are auto-exposed. If nil (the default), notifications are skipped.
func (r *Reconciler) SetMessageClient(mc MessageClient) {
	r.msgClient = mc
}

// Run starts the reconciliation loop. It blocks until ctx is cancelled.
//
// While reconciles succeed they run every cfg.Interval. After a failure the
// delay doubles per consecutive failure up to the backoff cap, and a 401
// from the hub moves straight to the cap: the agent token can be refreshed
// in place while the container runs, so the loop keeps retrying slowly
// rather than stopping. A successful reconcile restores the base interval.
func (r *Reconciler) Run(ctx context.Context) {
	for {
		delay := r.nextDelay(r.reconcileOnce(ctx))
		select {
		case <-ctx.Done():
			return
		case <-r.after(delay):
		}
	}
}

// nextDelay records the outcome of a reconcile, logs state changes, and
// returns the delay before the next one.
func (r *Reconciler) nextDelay(err error) time.Duration {
	if err == nil {
		if r.state != stateHealthy {
			log.Info("auto-expose: hub requests succeeded again; resuming every %s", r.cfg.Interval)
		}
		r.state = stateHealthy
		r.backoff.Reset()
		return r.cfg.Interval
	}
	if scionhub.IsUnauthorized(err) {
		delay := r.backoff.AtMax()
		if r.state != stateUnauthorized {
			log.Error("auto-expose: hub refused the agent credential (401); retrying every %s until it is accepted", r.backoff.Max)
		}
		r.state = stateUnauthorized
		return delay
	}
	delay := r.backoff.Failure()
	if r.state != stateBackoff {
		log.Error("auto-expose: reconcile failed: %v; backing off (up to %s between attempts)", err, r.backoff.Max)
	} else {
		log.Debug("auto-expose: reconcile failed: %v; next attempt in %s", err, delay)
	}
	r.state = stateBackoff
	return delay
}

// reconcileOnce runs one scan-and-reconcile pass. It returns the first error
// from the scan or the hub; a 401 ends the pass early since further hub
// calls with the same credential would be refused too.
func (r *Reconciler) reconcileOnce(ctx context.Context) error {
	// 1. Scan for listening sockets.
	sockets, err := r.scan()
	if err != nil {
		return fmt.Errorf("scan failed: %w", err)
	}

	// 2. Filter through policy.
	wanted := r.filterPorts(sockets)

	// 3. Get currently registered ports (from cache or hub).
	registered, err := r.getRegistered(ctx, false)
	if err != nil {
		return fmt.Errorf("failed to list registered ports: %w", err)
	}

	// 4. Diff: ports to expose = wanted - registered.
	toExpose := r.diffToExpose(wanted, registered)

	// 5. Diff: ports to unexpose = autoExposed - wanted (only ports we registered).
	toUnexpose := r.diffToUnexpose(wanted)

	var firstErr error
	fail := func(err error) bool {
		if firstErr == nil {
			firstErr = err
		}
		return scionhub.IsUnauthorized(err)
	}

	// 6. Register new ports.
	changed := false
	for _, port := range toExpose {
		if err := r.registerPort(ctx, port); err != nil {
			log.Debug("auto-expose: failed to register port %d: %v", port, err)
			if fail(fmt.Errorf("failed to register port %d: %w", port, err)) {
				return firstErr
			}
			continue
		}
		changed = true
	}

	// 7. Deregister stale ports.
	for _, port := range toUnexpose {
		if err := r.deregisterPort(ctx, port); err != nil {
			log.Debug("auto-expose: failed to deregister port %d: %v", port, err)
			if fail(fmt.Errorf("failed to deregister port %d: %w", port, err)) {
				return firstErr
			}
			continue
		}
		changed = true
	}

	// Refresh cache if we made changes.
	if changed {
		if _, err := r.getRegistered(ctx, true); err != nil {
			log.Debug("auto-expose: cache refresh after changes failed: %v", err)
		}
	}
	return firstErr
}

// filterPorts applies the configured policy to scanned sockets, returning
// the set of port numbers that should be exposed.
func (r *Reconciler) filterPorts(sockets []ListenSocket) map[int]bool {
	result := make(map[int]bool)

	deniedSet := make(map[int]bool, len(r.cfg.DeniedPorts))
	for _, p := range r.cfg.DeniedPorts {
		deniedSet[p] = true
	}

	filterSet := make(map[int]bool, len(r.cfg.FilterPorts))
	for _, p := range r.cfg.FilterPorts {
		filterSet[p] = true
	}

	for _, s := range sockets {
		port := s.Port

		// Always deny infrastructure ports.
		if deniedSet[port] {
			continue
		}

		// Minimum port check.
		if port < r.cfg.MinPort {
			continue
		}

		// Apply filter mode.
		switch r.cfg.FilterMode {
		case FilterModeAllowlist:
			if !filterSet[port] {
				continue
			}
		case FilterModeDenylist:
			if filterSet[port] {
				continue
			}
		}

		result[port] = true
	}

	return result
}

// diffToExpose returns ports that are wanted but not yet registered.
func (r *Reconciler) diffToExpose(wanted map[int]bool, registered map[int]string) []int {
	// Sort wanted ports for deterministic exposure order when exceeding MaxPorts.
	sorted := make([]int, 0, len(wanted))
	for port := range wanted {
		sorted = append(sorted, port)
	}
	sort.Ints(sorted)

	// Count currently auto-exposed ports.
	autoCount := len(r.autoExposed)

	var result []int
	for _, port := range sorted {
		if _, exists := registered[port]; exists {
			continue
		}
		// Respect max ports limit.
		if autoCount+len(result) >= r.cfg.MaxPorts {
			break
		}
		result = append(result, port)
	}
	return result
}

// diffToUnexpose returns ports that we auto-exposed but are no longer wanted.
// Never includes manually-exposed ports.
func (r *Reconciler) diffToUnexpose(wanted map[int]bool) []int {
	var result []int
	for port := range r.autoExposed {
		if !wanted[port] {
			result = append(result, port)
		}
	}
	return result
}

// getRegistered returns the currently registered ports, using the cache when
// possible. Pass force=true to always query the Hub.
func (r *Reconciler) getRegistered(ctx context.Context, force bool) (map[int]string, error) {
	r.ticksSinceRefresh++

	if !force && r.cachedRegistered != nil && r.ticksSinceRefresh < cacheRefreshInterval {
		return r.cachedRegistered, nil
	}

	ports, err := r.client.ListPorts(ctx)
	if err != nil {
		return r.cachedRegistered, err
	}

	registered := make(map[int]string, len(ports))
	for _, p := range ports {
		registered[p.Port] = p.ExposedBy
	}

	r.cachedRegistered = registered
	r.ticksSinceRefresh = 0
	return registered, nil
}

// registerPort registers a port with the Hub and tracks it as auto-exposed.
func (r *Reconciler) registerPort(ctx context.Context, port int) error {
	exposed, err := r.client.RegisterPort(ctx, scionhub.RegisterPortRequest{
		Port:  port,
		Label: autoExposeLabel,
		Host:  "127.0.0.1",
	})
	if err != nil {
		// Treat 409 Conflict as success — port is already registered.
		if isConflictError(err) {
			log.Debug("auto-expose: port %d already registered (409 conflict)", port)
			// Do NOT track as auto-exposed — the port may have been manually registered.
			// The next cache refresh will pick it up in 'registered' and diffToExpose
			// will skip it.
			return nil
		}
		return err
	}

	r.autoExposed[port] = true
	log.Info("auto-expose: exposed port %d", port)

	// Guard against RegisterPort returning (nil, nil).
	if exposed == nil {
		log.Info("auto-expose: registered port %d but received no exposed-port metadata; skipping notification", port)
		return nil
	}

	// Notify the agent that a port was auto-exposed.
	if r.msgClient != nil {
		notifyMsg := fmt.Sprintf("Port %d has been auto-exposed and is available at: %s — if you are collaborating with a user, consider sharing this URL so they can access what is running on this port.", port, exposed.URL)
		metadata := map[string]string{"system_category": messages.SystemCategoryPortForward}
		if err := r.msgClient.SendSelfMessage(ctx, notifyMsg, metadata); err != nil {
			log.Error("auto-expose: failed to send notification for port %d: %v", port, err)
		}
	}

	return nil
}

// deregisterPort removes a port from the Hub and stops tracking it.
func (r *Reconciler) deregisterPort(ctx context.Context, port int) error {
	if err := r.client.DeletePort(ctx, port); err != nil {
		return err
	}

	delete(r.autoExposed, port)
	log.Info("auto-expose: unexposed port %d", port)
	return nil
}

// isConflictError checks if an error is a 409 Conflict from the Hub.
func isConflictError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "error 409:")
}
