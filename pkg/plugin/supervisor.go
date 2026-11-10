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

package plugin

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
)

// SupervisionState is the supervisor's view of a broker plugin's connection.
type SupervisionState string

const (
	// SupervisionRunning: the plugin's session is up.
	SupervisionRunning SupervisionState = "running"
	// SupervisionRestarting: the session was lost; a respawn (hub-managed)
	// or re-dial (self-managed) is scheduled or in progress.
	SupervisionRestarting SupervisionState = "restarting"
	// SupervisionFailing: like restarting, after failingAfter consecutive
	// failed attempts.
	SupervisionFailing SupervisionState = "failing"
)

// SupervisionStatus is a snapshot of one supervised broker plugin.
type SupervisionStatus struct {
	State SupervisionState
	// Attempt is the backoff attempt counter. It counts restart attempts
	// since the plugin last stayed up for the reset period.
	Attempt int
	// Restarts counts successful respawns or re-dials since hub start.
	Restarts int
	// DroppedTotal counts publishes dropped for this plugin because its
	// connection failed, since hub start. It is never reset.
	DroppedTotal int64
	// DroppedThisOutage counts the publishes dropped since the plugin was
	// last running. It is reset to 0 when the plugin is running again.
	DroppedThisOutage int64
	// LastError is a fixed description of the last loss or failed attempt.
	LastError string
	// Since is when State last changed.
	Since time.Time
}

// BrokerConfigProvider returns the full config a broker plugin is restarted
// or re-dialled with (file config, secrets and hub wiring credentials).
type BrokerConfigProvider func(ctx context.Context, name string) (map[string]string, error)

// BrokerRestartHook is called after a broker plugin was respawned or
// re-dialled. The hub uses it to replace the plugin's fan-out spoke and
// replay its subscriptions. An error keeps the plugin restarting.
type BrokerRestartHook func(name string) error

// Supervisor timing. Variables so tests can shorten them.
var (
	supervisorCheckInterval = 10 * time.Second
	supervisorPingTimeout   = 5 * time.Second
	// supervisorPingTimeouts consecutive Ping timeouts mark a session lost.
	supervisorPingTimeouts = 2
	backoffInitial         = time.Second
	backoffMax             = 5 * time.Minute
	// backoffResetAfter is how long a restarted plugin must stay up before
	// the attempt counter resets.
	backoffResetAfter = 10 * time.Minute
	// failingAfter consecutive failed attempts report the plugin failing.
	failingAfter = 5
	// dropWarnInterval rate-limits the dropped-publish warning per plugin.
	dropWarnInterval = 10 * time.Second
	// restartStepTimeout bounds each step of a restart attempt that calls
	// out (config provider, plugin Configure, restart hooks), so an
	// attempt always ends and releases the plugin's restart lock.
	restartStepTimeout = 60 * time.Second
	// gracefulStopTimeout bounds a graceful stop of a hub-managed plugin
	// (go-plugin's Kill sends a Quit request with no deadline first);
	// past it the process is killed.
	gracefulStopTimeout = 5 * time.Second
)

// backoffDelay returns the delay after restart attempt n (n >= 1):
// backoffInitial doubled per attempt, capped at backoffMax, with jitter r in
// [0, 1) scaling it by 0.8 to 1.2.
func backoffDelay(n int, r float64) time.Duration {
	if n < 1 {
		n = 1
	}
	d := backoffInitial
	for i := 1; i < n && d < backoffMax; i++ {
		d *= 2
	}
	if d > backoffMax {
		d = backoffMax
	}
	return time.Duration(float64(d) * (0.8 + 0.4*r))
}

// supervised is the supervisor's record of one broker plugin, keyed like
// Manager.clients. Fields are guarded by Manager.sup.mu.
type supervised struct {
	status SupervisionStatus

	// upSince is when the plugin was last (re)started successfully.
	upSince time.Time
	// failures counts consecutive failed attempts.
	failures int
	// nextAttempt is when the next restart attempt may start. A kick or a
	// check never moves it earlier.
	nextAttempt time.Time
	// lostClient is the client judged lost; an attempt acts only while it
	// is still the current client.
	lostClient *goplugin.Client
	// busy is set while a probe or attempt runs for this plugin.
	busy bool
	// pingTimeouts counts consecutive Ping timeouts.
	pingTimeouts int
	lastDropWarn time.Time
}

// resetAttemptsIfStable resets the attempt counter of a running plugin that
// has stayed up for backoffResetAfter, and reports whether it did.
func (sp *supervised) resetAttemptsIfStable(now time.Time) bool {
	if sp.status.State != SupervisionRunning || sp.status.Attempt == 0 || now.Sub(sp.upSince) < backoffResetAfter {
		return false
	}
	sp.status.Attempt = 0
	return true
}

// supervisor holds the Manager's supervision state.
type supervisor struct {
	mu       sync.Mutex
	plugins  map[string]*supervised
	provider BrokerConfigProvider
	hooks    []BrokerRestartHook
	kick     chan struct{}
	started  bool
	// wg counts the supervisor loop and its probe and attempt goroutines.
	wg     sync.WaitGroup
	now    func() time.Time
	jitter func() float64
}

func newSupervisor() *supervisor {
	return &supervisor{
		plugins: make(map[string]*supervised),
		kick:    make(chan struct{}, 1),
		now:     time.Now,
		jitter:  rand.Float64,
	}
}

// SetBrokerConfigProvider sets the source of the config used when a broker
// plugin is respawned, re-dialled or reconnected. Without one, the plugin's
// stored config is used.
func (m *Manager) SetBrokerConfigProvider(p BrokerConfigProvider) {
	m.sup.mu.Lock()
	m.sup.provider = p
	m.sup.mu.Unlock()
}

// OnBrokerRestart registers a hook called after a broker plugin is
// respawned or re-dialled by the supervisor, or reconnected by Reconnect.
func (m *Manager) OnBrokerRestart(h BrokerRestartHook) {
	m.sup.mu.Lock()
	m.sup.hooks = append(m.sup.hooks, h)
	m.sup.mu.Unlock()
}

// BrokerSupervision returns the supervision status of the named broker
// plugin, or false when it is not supervised (not loaded, or gRPC/HA).
func (m *Manager) BrokerSupervision(name string) (SupervisionStatus, bool) {
	key := PluginTypeBroker + ":" + name
	m.sup.mu.Lock()
	defer m.sup.mu.Unlock()
	sp, ok := m.sup.plugins[key]
	if !ok {
		return SupervisionStatus{}, false
	}
	return sp.status, true
}

// StartSupervisor starts watching the loaded hub-managed and self-managed
// broker plugins until ctx ends or shutdown begins. A lost session (process
// exit, failed or hung Ping, transport error on a call) is recovered by a
// respawn (hub-managed) or re-dial (self-managed) with backoff, then the
// restart hooks run. It is a no-op after the first call.
func (m *Manager) StartSupervisor(ctx context.Context) {
	m.sup.mu.Lock()
	if m.sup.started {
		m.sup.mu.Unlock()
		return
	}
	m.sup.started = true
	m.sup.wg.Add(1)
	m.sup.mu.Unlock()

	go func() {
		defer m.sup.wg.Done()
		ticker := time.NewTicker(supervisorCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-m.sup.kick:
			}
			if m.stopping.Load() {
				return
			}
			m.superviseOnce(ctx)
		}
	}()
}

// waitSupervisorStopped waits until the supervisor loop has returned (its
// ctx ended or shutdown began) and its probes and attempts have finished.
func (m *Manager) waitSupervisorStopped() {
	m.sup.wg.Wait()
}

// kickSupervisor asks for an immediate check without blocking.
func (m *Manager) kickSupervisor() {
	select {
	case m.sup.kick <- struct{}{}:
	default:
	}
}

// supervisedLoaded records that the plugin key is running: loaded by
// LoadAll/LoadOne or an admin restart, or restarted by the supervisor with
// its hooks done. The attempt counter is kept (it resets only after the
// plugin stays up for backoffResetAfter).
func (m *Manager) supervisedLoaded(key string) {
	now := m.sup.now()
	m.sup.mu.Lock()
	defer m.sup.mu.Unlock()
	sp, ok := m.sup.plugins[key]
	if !ok {
		sp = &supervised{}
		m.sup.plugins[key] = sp
	}
	if sp.status.State != SupervisionRunning {
		sp.status.State = SupervisionRunning
		sp.status.Since = now
	}
	sp.status.DroppedThisOutage = 0
	sp.lostClient = nil
	sp.pingTimeouts = 0
	sp.failures = 0
	sp.upSince = now
}

// transportErrorHandler returns the BrokerRPCClient transport-error callback
// for the plugin key: it counts a failed Publish as dropped (also on a
// discarded client, which a fan-out spoke keeps using until the plugin is
// restarted) and, for a client still in use, kicks the supervisor, which
// then checks the session (a kick alone never restarts).
func (m *Manager) transportErrorHandler(key string) func(method, topic, recipient string, stale bool, err error) {
	return func(method, topic, recipient string, stale bool, err error) {
		if method == "Publish" {
			m.recordDrop(key, topic, recipient)
		}
		if !stale {
			m.kickSupervisor()
		}
	}
}

// recordDrop counts a publish dropped for the plugin key and logs it,
// rate-limited, without the message content.
func (m *Manager) recordDrop(key, topic, recipient string) {
	now := m.sup.now()
	m.sup.mu.Lock()
	sp, ok := m.sup.plugins[key]
	if !ok {
		m.sup.mu.Unlock()
		return
	}
	sp.status.DroppedTotal++
	sp.status.DroppedThisOutage++
	outage := sp.status.DroppedThisOutage
	warn := now.Sub(sp.lastDropWarn) >= dropWarnInterval
	if warn {
		sp.lastDropWarn = now
	}
	m.sup.mu.Unlock()
	if warn {
		m.logger.Warn("Broker plugin publish dropped: connection failed",
			"plugin", key, "topic", topic, "recipient", recipient, "dropped_this_outage", outage)
	}
}

// superviseOnce checks every supervised plugin once: running plugins are
// probed, lost ones get a restart attempt when their backoff has passed.
// Probes and attempts run in their own goroutines, one at a time per
// plugin.
func (m *Manager) superviseOnce(ctx context.Context) {
	now := m.sup.now()
	type job struct {
		key     string
		client  *goplugin.Client
		self    bool
		attempt bool
	}
	var jobs []job

	kick := false
	m.mu.RLock()
	m.sup.mu.Lock()
	for key, sp := range m.sup.plugins {
		if sp.busy {
			continue
		}
		client, hasClient := m.clients[key]
		if _, hasConfig := m.configs[key]; !hasConfig && !hasClient {
			// Unloaded (for example by Shutdown): nothing to supervise.
			delete(m.sup.plugins, key)
			continue
		}
		self := m.selfManaged[key]
		switch sp.status.State {
		case SupervisionRunning:
			if !hasClient {
				// A load failed after the client was dropped (Reconnect
				// or an admin restart): recover it like a lost session.
				m.markLostLocked(sp, key, nil, "plugin not loaded", now)
				kick = true
				continue
			}
			sp.resetAttemptsIfStable(now)
			sp.busy = true
			jobs = append(jobs, job{key: key, client: client, self: self})
		default:
			// A plugin whose last attempt failed to load has no client;
			// it is retried from its stored config.
			if now.Before(sp.nextAttempt) {
				continue
			}
			sp.busy = true
			jobs = append(jobs, job{key: key, client: client, self: self, attempt: true})
		}
	}
	m.sup.mu.Unlock()
	m.mu.RUnlock()
	if kick {
		m.kickSupervisor()
	}

	for _, j := range jobs {
		m.sup.wg.Add(1)
		go func() {
			defer m.sup.wg.Done()
			lost := false
			defer func() {
				m.sup.mu.Lock()
				if sp, ok := m.sup.plugins[j.key]; ok {
					sp.busy = false
				}
				m.sup.mu.Unlock()
				// Kick only once busy is cleared, so the next pass can
				// start the attempt at once.
				if lost {
					m.kickSupervisor()
				}
			}()
			if j.attempt {
				m.restartAttempt(ctx, j.key)
				return
			}
			var reason string
			if lost, reason = m.probe(j.key, j.client, j.self); lost {
				m.markLost(j.key, j.client, reason)
			}
		}()
	}
}

// probe reports whether the plugin's session is lost, and why.
func (m *Manager) probe(key string, client *goplugin.Client, self bool) (bool, string) {
	// A self-managed (reattached) client reports Exited at once: its
	// runner does not own a process.
	if !self && client.Exited() {
		return true, "plugin process exited"
	}
	cp, err := client.Client()
	if err != nil {
		return true, "plugin client unavailable"
	}
	res := make(chan error, 1)
	go func() { res <- cp.Ping() }()
	timer := time.NewTimer(supervisorPingTimeout)
	defer timer.Stop()
	select {
	case err := <-res:
		m.sup.mu.Lock()
		if sp, ok := m.sup.plugins[key]; ok {
			sp.pingTimeouts = 0
		}
		m.sup.mu.Unlock()
		if err != nil {
			return true, "plugin session lost"
		}
		return false, ""
	case <-timer.C:
		m.sup.mu.Lock()
		defer m.sup.mu.Unlock()
		sp, ok := m.sup.plugins[key]
		if !ok {
			return false, ""
		}
		sp.pingTimeouts++
		if sp.pingTimeouts >= supervisorPingTimeouts {
			return true, "plugin session not responding"
		}
		return false, ""
	}
}

// markLost moves a running plugin to restarting, unless client is no
// longer its current client (already replaced). The first attempt after a
// loss is due at once when the attempt counter is 0; otherwise it waits
// backoffDelay(Attempt), so a plugin that keeps dying soon after each
// restart keeps backing off.
func (m *Manager) markLost(key string, client *goplugin.Client, reason string) {
	now := m.sup.now()
	m.mu.RLock()
	current := m.clients[key]
	m.mu.RUnlock()
	if current != client {
		return // already replaced: not a loss of the current client
	}
	m.sup.mu.Lock()
	sp, ok := m.sup.plugins[key]
	if !ok || sp.status.State != SupervisionRunning {
		m.sup.mu.Unlock()
		return
	}
	m.markLostLocked(sp, key, client, reason, now)
	m.sup.mu.Unlock()
}

// markLostLocked is markLost for a running plugin's record; m.sup.mu is held.
func (m *Manager) markLostLocked(sp *supervised, key string, client *goplugin.Client, reason string, now time.Time) {
	sp.status.State = SupervisionRestarting
	sp.status.Since = now
	sp.status.LastError = reason
	sp.lostClient = client
	sp.pingTimeouts = 0
	sp.failures = 0
	if sp.status.Attempt == 0 {
		sp.nextAttempt = now
	} else {
		sp.nextAttempt = now.Add(backoffDelay(sp.status.Attempt, m.sup.jitter()))
	}
	m.logger.Warn("Broker plugin connection lost; restart scheduled", "plugin", key, "reason", reason)
}

// restartAttempt makes one respawn or re-dial attempt for a lost plugin,
// serialized with the admin restart paths.
func (m *Manager) restartAttempt(ctx context.Context, key string) {
	lock := m.restartLock(key)
	lock.Lock()
	defer lock.Unlock()

	if m.stopping.Load() {
		return
	}

	m.mu.RLock()
	current, hasClient := m.clients[key]
	dp, hasConfig := m.configs[key]
	self := m.selfManaged[key]
	m.mu.RUnlock()

	m.sup.mu.Lock()
	sp, ok := m.sup.plugins[key]
	if !ok || sp.status.State == SupervisionRunning {
		m.sup.mu.Unlock()
		return
	}
	lost := sp.lostClient
	if hasClient && current != lost {
		// Already replaced (an admin Restart, Update or Reconnect): no
		// attempt is made or counted.
		m.sup.mu.Unlock()
		m.supervisedLoaded(key)
		return
	}
	sp.status.Attempt++
	attempt := sp.status.Attempt
	provider := m.sup.provider
	m.sup.mu.Unlock()
	if !hasConfig {
		m.attemptFailed(key, attempt, errors.New("no stored plugin config"))
		return
	}

	cfg := dp.Config
	if provider != nil {
		pctx, cancel := context.WithTimeout(ctx, restartStepTimeout)
		resolved, err := provider(pctx, dp.Name)
		cancel()
		if err != nil {
			m.attemptFailed(key, attempt, fmt.Errorf("resolve config: %w", err))
			return
		}
		cfg = resolved
	}

	m.logger.Info("Restarting broker plugin", "plugin", key, "attempt", attempt, "self_managed", self)
	m.closeStaleClient(key, lost)
	if err := m.loadPluginWithConfig(dp, cfg); err != nil {
		m.attemptFailed(key, attempt, err)
		return
	}
	if m.stopping.Load() {
		return
	}
	if err := m.runRestartHooksWithin(dp.Name, restartStepTimeout); err != nil {
		m.attemptFailed(key, attempt, fmt.Errorf("restart hook: %w", err))
		return
	}
	m.supervisedLoaded(key)
	m.sup.mu.Lock()
	if sp, ok := m.sup.plugins[key]; ok {
		sp.status.Restarts++
	}
	m.sup.mu.Unlock()
	m.logger.Info("Broker plugin restarted", "plugin", key, "attempt", attempt)
}

// closeStaleClient replaces a lost client (see replaceClient), without a
// graceful stop: its session is gone or hung.
func (m *Manager) closeStaleClient(key string, lost *goplugin.Client) {
	if lost == nil {
		return
	}
	m.replaceClient(key, lost, false)
}

// replaceClient is the one path by which a plugin's current client is
// taken out of service by the supervisor, Reconnect, or an admin restart
// or update. A load that supersedes a client and Shutdown, which take
// their clients out under the lock themselves, discard them through
// discardClient the same way. It removes the client stored for key if it
// is expect (or any client when expect is nil), discards it (see
// discardClient), and reports whether a client was removed.
func (m *Manager) replaceClient(key string, expect *goplugin.Client, graceful bool) bool {
	m.mu.Lock()
	client, ok := m.clients[key]
	if !ok || (expect != nil && client != expect) {
		m.mu.Unlock()
		return false
	}
	raw := m.dispensed[key]
	self := m.selfManaged[key]
	delete(m.clients, key)
	delete(m.dispensed, key)
	m.mu.Unlock()
	m.discardClient(key, client, raw, self, graceful)
	return true
}

// discardClient makes a client that is being replaced or abandoned unable to
// reach the plugin again. raw is its dispensed interface, if known. It
// never holds a Manager lock.
//
// Its dispensed broker connection is closed locally (no RPC is sent) and
// marked stale, so a fan-out adapter still wrapping it can no longer send
// Publish, Subscribe or Close to the plugin; for a self-managed plugin that
// instance is shared by every connection, so an old adapter's Close would
// otherwise close it for the new connection too. Its failed publishes are
// still counted as drops.
//
// A self-managed plugin's process is not the hub's: nothing more is done.
// Its control session is left to end on its own (go-plugin offers no way to
// close it without a Quit request); a healthy one stays open, one per
// re-dial, until the plugin process exits, together with any Ping or
// connect goroutine still blocked on it.
//
// A hub-managed plugin is stopped. When graceful is set, go-plugin's Kill
// (a Quit request, then a kill after a short wait) gets gracefulStopTimeout;
// if it has not returned by then (a hung session never answers the Quit),
// the process is killed, which ends the session and the pending Quit.
// Otherwise the process is killed at once first (a lost session may be
// hung). Either way the stop is bounded.
func (m *Manager) discardClient(key string, client *goplugin.Client, raw interface{}, self, graceful bool) {
	if bc, ok := raw.(*BrokerRPCClient); ok {
		bc.discard()
	}
	if self {
		return
	}
	if !graceful {
		killPluginProcess(client)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.Kill()
	}()
	timer := time.NewTimer(gracefulStopTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
	}
	if graceful {
		m.logger.Warn("Plugin did not stop within the graceful timeout; killing it", "plugin", key, "timeout", gracefulStopTimeout)
		killPluginProcess(client)
	}
	select {
	case <-done:
	case <-time.After(gracefulStopTimeout):
		m.logger.Error("Plugin client did not finish stopping after its process was killed", "plugin", key, "timeout", gracefulStopTimeout)
	}
}

// killPluginProcess kills a hub-managed plugin's process. Only a process
// not yet reaped is signalled, so the pid cannot have been reused.
func killPluginProcess(client *goplugin.Client) {
	if rc := client.ReattachConfig(); rc != nil && rc.Pid > 0 && !client.Exited() {
		if p, err := os.FindProcess(rc.Pid); err == nil {
			_ = p.Kill()
		}
	}
}

// attemptFailed schedules the next attempt backoffDelay(attempt) later:
// 1 s after the first failed attempt, doubling up to backoffMax.
func (m *Manager) attemptFailed(key string, attempt int, err error) {
	now := m.sup.now()
	m.mu.RLock()
	current := m.clients[key]
	m.mu.RUnlock()
	m.sup.mu.Lock()
	sp, ok := m.sup.plugins[key]
	if !ok {
		m.sup.mu.Unlock()
		return
	}
	sp.failures++
	state := SupervisionRestarting
	if sp.failures >= failingAfter {
		state = SupervisionFailing
	}
	if sp.status.State != state {
		sp.status.State = state
		sp.status.Since = now
	}
	sp.status.LastError = "restart attempt failed"
	// The plugin may have been reloaded before a hook failed; restart
	// whatever client is current next time.
	sp.lostClient = current
	delay := backoffDelay(attempt, m.sup.jitter())
	sp.nextAttempt = now.Add(delay)
	m.sup.mu.Unlock()
	m.logger.Warn("Broker plugin restart attempt failed",
		"plugin", key, "attempt", attempt, "next_in", delay, "error", err)
}

// runRestartHooks calls the restart hooks and joins their errors.
func (m *Manager) runRestartHooks(name string) error {
	m.sup.mu.Lock()
	hooks := append([]BrokerRestartHook(nil), m.sup.hooks...)
	m.sup.mu.Unlock()
	var errs []error
	for _, h := range hooks {
		if err := h(name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// runRestartHooksWithin runs the restart hooks, giving up after timeout. A
// hook still running then (for example a subscription replay on a plugin
// that does not answer) finishes in the background; the next attempt
// replaces that plugin, which fails its pending calls.
func (m *Manager) runRestartHooksWithin(name string, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- m.runRestartHooks(name) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("restart hooks did not finish within %v", timeout)
	}
}

// restartLock returns the per-plugin lock that serializes supervisor
// attempts with the admin restart paths.
func (m *Manager) restartLock(key string) *sync.Mutex {
	l, _ := m.restartLocks.LoadOrStore(key, &sync.Mutex{})
	return l.(*sync.Mutex)
}
