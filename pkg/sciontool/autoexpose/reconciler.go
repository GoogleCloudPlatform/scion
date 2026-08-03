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
}

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
	}
}

// SetMessageClient sets the optional message client used to notify the agent
// when ports are auto-exposed. If nil (the default), notifications are skipped.
func (r *Reconciler) SetMessageClient(mc MessageClient) {
	r.msgClient = mc
}

// Run starts the reconciliation loop. It blocks until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()

	// Do an initial reconcile immediately.
	r.reconcileOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reconcileOnce(ctx)
		}
	}
}

func (r *Reconciler) reconcileOnce(ctx context.Context) {
	// 1. Scan for listening sockets.
	sockets, err := r.scan()
	if err != nil {
		log.Error("auto-expose: scan failed: %v", err)
		return
	}

	// 2. Filter through policy.
	wanted := r.filterPorts(sockets)

	// 3. Get currently registered ports (from cache or hub).
	registered, err := r.getRegistered(ctx, false)
	if err != nil {
		log.Error("auto-expose: failed to list registered ports: %v", err)
		return
	}

	// 4. Diff: ports to expose = wanted - registered.
	toExpose := r.diffToExpose(wanted, registered)

	// 5. Diff: ports to unexpose = autoExposed - wanted (only ports we registered).
	toUnexpose := r.diffToUnexpose(wanted)

	// 6. Register new ports.
	changed := false
	for _, port := range toExpose {
		if err := r.registerPort(ctx, port); err != nil {
			log.Error("auto-expose: failed to register port %d: %v", port, err)
			continue
		}
		changed = true
	}

	// 7. Deregister stale ports.
	for _, port := range toUnexpose {
		if err := r.deregisterPort(ctx, port); err != nil {
			log.Error("auto-expose: failed to deregister port %d: %v", port, err)
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
